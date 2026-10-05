// Package store keeps the board in SQLite: tasks, comments, events and usage.
//
// The board service is the only writer. Every change to a task is recorded as
// an event in the same transaction, so the history can always explain the
// current state.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// Task states, as in the design doc's lifecycle.
const (
	Queued     = "queued"
	Working    = "working"
	Blocked    = "blocked" // waiting for its subtasks to reach review
	Review     = "review"
	Done       = "done"
	NeedsHuman = "needs_human"
)

// States in board order, for listings and the web view.
var States = []string{Queued, Working, Blocked, Review, NeedsHuman, Done}

// transitions lists the allowed moves. Anything else is refused, so a buggy
// worker cannot, say, mark a queued task done.
var transitions = map[string][]string{
	Queued:     {Working},
	Working:    {Review, Queued, NeedsHuman, Blocked},
	Blocked:    {Queued, NeedsHuman},
	Review:     {Done, Queued},
	NeedsHuman: {Queued},
	Done:       {},
}

// Delegation limits: how deep subtasks may nest and how many one task may have.
const (
	MaxDepth    = 3
	MaxChildren = 10
)

var (
	ErrNotFound   = errors.New("not found")
	ErrTransition = errors.New("transition not allowed")
	ErrInvalid    = errors.New("invalid request")
)

type Task struct {
	ID           int64           `json:"id"`
	Title        string          `json:"title"`
	Body         string          `json:"body"`
	Repo         string          `json:"repo"`
	BaseRev      string          `json:"base_rev"`
	State        string          `json:"state"`
	Priority     int             `json:"priority"`
	Labels       json.RawMessage `json:"labels"`
	Assignee     string          `json:"assignee"`
	ResultBranch string          `json:"result_branch"`
	ParentID     *int64          `json:"parent_id"`
	CreatedBy    string          `json:"created_by"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

type Comment struct {
	ID        int64  `json:"id"`
	TaskID    int64  `json:"task_id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

type Event struct {
	ID     int64           `json:"id"`
	TaskID int64           `json:"task_id"`
	Kind   string          `json:"kind"`
	Actor  string          `json:"actor"`
	Data   json.RawMessage `json:"data"`
	At     string          `json:"at"`
}

type Usage struct {
	TaskID     int64   `json:"task_id"`
	Runtime    string  `json:"runtime"`
	Backend    string  `json:"backend"`
	Model      string  `json:"model"`
	Input      int64   `json:"input"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Output     int64   `json:"output"`
	WallS      float64 `json:"wall_s"`
}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (and if needed creates) the board database at path.
func Open(path string) (*Store, error) {
	// WAL lets readers proceed while the one writer commits; Litestream also needs it.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; serialises transactions without SQLITE_BUSY
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) stamp() string { return s.now().UTC().Format(time.RFC3339) }

const taskCols = `id, title, body, repo, base_rev, state, priority, labels_json, assignee,
	result_branch, parent_id, created_by, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanTask(r scanner) (*Task, error) {
	var t Task
	var labels string
	var parent sql.NullInt64
	err := r.Scan(&t.ID, &t.Title, &t.Body, &t.Repo, &t.BaseRev, &t.State, &t.Priority, &labels,
		&t.Assignee, &t.ResultBranch, &parent, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Labels = json.RawMessage(labels)
	if parent.Valid {
		t.ParentID = &parent.Int64
	}
	return &t, nil
}

func event(ctx context.Context, tx *sql.Tx, task int64, kind, actor string, data any, at string) error {
	js, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events (task_id, kind, actor, data_json, at) VALUES (?, ?, ?, ?, ?)`,
		task, kind, actor, string(js), at)
	return err
}

// NewTask is what a person or an agent submits.
type NewTask struct {
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Repo      string          `json:"repo"`
	BaseRev   string          `json:"base_rev"`
	Priority  int             `json:"priority"`
	Labels    json.RawMessage `json:"labels"`
	ParentID  *int64          `json:"parent_id"`
	CreatedBy string          `json:"created_by"`
}

func (s *Store) CreateTask(ctx context.Context, n NewTask) (*Task, error) {
	if strings.TrimSpace(n.Title) == "" || n.CreatedBy == "" {
		return nil, fmt.Errorf("%w: title and created_by are required", ErrInvalid)
	}
	labels := "{}"
	if len(n.Labels) > 0 && string(n.Labels) != "null" { // an absent field arrives as JSON null
		if !json.Valid(n.Labels) {
			return nil, fmt.Errorf("%w: labels must be JSON", ErrInvalid)
		}
		labels = string(n.Labels)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if n.ParentID != nil {
		if err := checkDelegation(ctx, tx, *n.ParentID); err != nil {
			return nil, err
		}
	}
	at := s.stamp()
	res, err := tx.ExecContext(ctx, `INSERT INTO tasks (title, body, repo, base_rev, priority, labels_json,
		parent_id, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.Title, n.Body, n.Repo, n.BaseRev, n.Priority, labels, n.ParentID, n.CreatedBy, at, at)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := event(ctx, tx, id, "created", n.CreatedBy, map[string]any{"parent_id": n.ParentID}, at); err != nil {
		return nil, err
	}
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return t, tx.Commit()
}

// checkDelegation enforces the depth and fan-out caps on subtasks.
func checkDelegation(ctx context.Context, tx *sql.Tx, parent int64) error {
	depth := 1
	for id := (sql.NullInt64{Int64: parent, Valid: true}); id.Valid; depth++ {
		var next sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT parent_id FROM tasks WHERE id = ?`, id.Int64).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: parent task %d", ErrNotFound, parent)
		}
		if err != nil {
			return err
		}
		id = next
	}
	if depth > MaxDepth {
		return fmt.Errorf("%w: subtasks may nest at most %d deep", ErrInvalid, MaxDepth)
	}
	var children int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE parent_id = ?`, parent).Scan(&children); err != nil {
		return err
	}
	if children >= MaxChildren {
		return fmt.Errorf("%w: task %d already has %d subtasks", ErrInvalid, parent, MaxChildren)
	}
	return nil
}

func (s *Store) GetTask(ctx context.Context, id int64) (*Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
}

// ListFilter narrows ListTasks; zero values mean "any".
type ListFilter struct {
	State    string
	ParentID *int64
	Assignee string
}

// ListTasks returns tasks in pick-up order: highest priority first, then oldest.
func (s *Store) ListTasks(ctx context.Context, f ListFilter) ([]Task, error) {
	q := `SELECT ` + taskCols + ` FROM tasks WHERE 1=1`
	var args []any
	if f.State != "" {
		q += ` AND state = ?`
		args = append(args, f.State)
	}
	if f.ParentID != nil {
		q += ` AND parent_id = ?`
		args = append(args, *f.ParentID)
	}
	if f.Assignee != "" {
		q += ` AND assignee = ?`
		args = append(args, f.Assignee)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY priority DESC, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// Update is a partial change to a task; nil fields stay as they are.
type Update struct {
	State        *string          `json:"state"`
	Assignee     *string          `json:"assignee"`
	Priority     *int             `json:"priority"`
	Labels       *json.RawMessage `json:"labels"`
	ResultBranch *string          `json:"result_branch"`
	Actor        string           `json:"actor"`
}

func (s *Store) UpdateTask(ctx context.Context, id int64, u Update) (*Task, error) {
	if u.Actor == "" {
		return nil, fmt.Errorf("%w: actor is required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	at := s.stamp()
	if u.Assignee != nil && *u.Assignee != t.Assignee {
		if err := event(ctx, tx, id, "assigned", u.Actor, map[string]string{"from": t.Assignee, "to": *u.Assignee}, at); err != nil {
			return nil, err
		}
		t.Assignee = *u.Assignee
	}
	if u.State != nil && *u.State != t.State {
		if !allowed(t.State, *u.State) {
			return nil, fmt.Errorf("%w: %s -> %s", ErrTransition, t.State, *u.State)
		}
		if *u.State == Working && t.Assignee == "" {
			return nil, fmt.Errorf("%w: a task needs an assignee to start working", ErrInvalid)
		}
		if *u.State == Queued {
			t.Assignee = "" // back in the pool: nobody holds it any more
		}
		if err := event(ctx, tx, id, "state", u.Actor, map[string]string{"from": t.State, "to": *u.State}, at); err != nil {
			return nil, err
		}
		t.State = *u.State
	}
	if u.Priority != nil {
		t.Priority = *u.Priority
	}
	if u.Labels != nil {
		if !json.Valid(*u.Labels) {
			return nil, fmt.Errorf("%w: labels must be JSON", ErrInvalid)
		}
		t.Labels = *u.Labels
	}
	if u.ResultBranch != nil {
		t.ResultBranch = *u.ResultBranch
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, assignee = ?, priority = ?, labels_json = ?,
		result_branch = ?, updated_at = ? WHERE id = ?`,
		t.State, t.Assignee, t.Priority, string(t.Labels), t.ResultBranch, at, id); err != nil {
		return nil, err
	}
	t.UpdatedAt = at
	if u.State != nil && t.ParentID != nil {
		if err := s.resumeParent(ctx, tx, *t.ParentID, at); err != nil {
			return nil, err
		}
	}
	return t, tx.Commit()
}

func allowed(from, to string) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// resumeParent re-queues a blocked parent once every subtask has reached
// review or done, so a worker can pick it up and assemble their branches.
func (s *Store) resumeParent(ctx context.Context, tx *sql.Tx, parent int64, at string) error {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id = ?`, parent).Scan(&state); err != nil {
		return err
	}
	if state != Blocked {
		return nil
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE parent_id = ? AND state NOT IN (?, ?)`,
		parent, Review, Done).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state = ?, assignee = '', updated_at = ? WHERE id = ?`,
		Queued, at, parent); err != nil {
		return err
	}
	return event(ctx, tx, parent, "state", "board", map[string]string{"from": Blocked, "to": Queued, "reason": "subtasks ready"}, at)
}

func (s *Store) AddComment(ctx context.Context, task int64, author, body string) (*Comment, error) {
	if author == "" || strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: author and body are required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, task)); err != nil {
		return nil, err
	}
	at := s.stamp()
	res, err := tx.ExecContext(ctx, `INSERT INTO comments (task_id, author, body, created_at) VALUES (?, ?, ?, ?)`,
		task, author, body, at)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := event(ctx, tx, task, "comment", author, map[string]int64{"comment_id": id}, at); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, at, task); err != nil {
		return nil, err
	}
	return &Comment{ID: id, TaskID: task, Author: author, Body: body, CreatedAt: at}, tx.Commit()
}

func (s *Store) Comments(ctx context.Context, task int64) ([]Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, author, body, created_at FROM comments
		WHERE task_id = ? ORDER BY id`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Author, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SearchComments runs an FTS5 query over all comments, best match first.
func (s *Store) SearchComments(ctx context.Context, query string) ([]Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.task_id, c.author, c.body, c.created_at
		FROM comments_fts f JOIN comments c ON c.id = f.rowid WHERE comments_fts MATCH ? ORDER BY rank LIMIT 50`, query)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Author, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Events(ctx context.Context, task int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, kind, actor, data_json, at FROM events
		WHERE task_id = ? ORDER BY id`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var data string
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Kind, &e.Actor, &data, &e.At); err != nil {
			return nil, err
		}
		e.Data = json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) RecordUsage(ctx context.Context, u Usage, actor string) error {
	if u.Runtime == "" || u.Backend == "" || u.Model == "" {
		return fmt.Errorf("%w: runtime, backend and model are required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, u.TaskID)); err != nil {
		return err
	}
	at := s.stamp()
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage (task_id, runtime, backend, model, input, cache_read,
		cache_write, output, wall_s, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.TaskID, u.Runtime, u.Backend, u.Model, u.Input, u.CacheRead, u.CacheWrite, u.Output, u.WallS, at); err != nil {
		return err
	}
	if err := event(ctx, tx, u.TaskID, "usage", actor, u, at); err != nil {
		return err
	}
	return tx.Commit()
}

// UsageTotals sums a task's usage per runtime, backend and model.
func (s *Store) UsageTotals(ctx context.Context, task int64) ([]Usage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT runtime, backend, model, sum(input), sum(cache_read),
		sum(cache_write), sum(output), sum(wall_s) FROM usage WHERE task_id = ?
		GROUP BY runtime, backend, model ORDER BY runtime, backend, model`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Usage{}
	for rows.Next() {
		u := Usage{TaskID: task}
		if err := rows.Scan(&u.Runtime, &u.Backend, &u.Model, &u.Input, &u.CacheRead, &u.CacheWrite,
			&u.Output, &u.WallS); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
