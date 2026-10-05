// Package api serves the board over HTTP: a JSON API for the CLI and agents,
// and a read-only board page for people.
package api

import (
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"smurfs-village/smurf/internal/store"
)

//go:embed board.html
var pages embed.FS

var boardPage = template.Must(template.New("board.html").Funcs(template.FuncMap{
	"title": func(s string) string { return strings.ToUpper(s[:1]) + strings.ReplaceAll(s[1:], "_", " ") },
}).ParseFS(pages, "board.html"))

type Server struct {
	Store *store.Store
	// Token, when set, is required as "Authorization: Bearer <token>" on the
	// API. In the cluster the board sits behind the security proxy instead.
	Token string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.board)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	api := http.NewServeMux()
	api.HandleFunc("POST /api/tasks", s.createTask)
	api.HandleFunc("GET /api/tasks", s.listTasks)
	api.HandleFunc("GET /api/tasks/{id}", s.getTask)
	api.HandleFunc("PATCH /api/tasks/{id}", s.updateTask)
	api.HandleFunc("POST /api/tasks/{id}/comments", s.addComment)
	api.HandleFunc("GET /api/tasks/{id}/events", s.events)
	api.HandleFunc("POST /api/tasks/{id}/usage", s.recordUsage)
	api.HandleFunc("GET /api/search", s.search)
	mux.Handle("/api/", s.auth(api))
	return mux
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" && r.Header.Get("Authorization") != "Bearer "+s.Token {
			fail(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]string{"error": msg})
}

// failErr maps store errors onto HTTP statuses.
func failErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrTransition):
		fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInvalid):
		fail(w, http.StatusBadRequest, err.Error())
	default:
		log.Printf("internal error: %v", err)
		fail(w, http.StatusInternalServerError, "internal error")
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return false
	}
	return true
}

func taskID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "task id must be a number")
		return 0, false
	}
	return id, true
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var n store.NewTask
	if !decode(w, r, &n) {
		return
	}
	t, err := s.Store.CreateTask(r.Context(), n)
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusCreated, t)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ListFilter{State: q.Get("state"), Assignee: q.Get("assignee")}
	if p := q.Get("parent"); p != "" {
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			fail(w, http.StatusBadRequest, "parent must be a number")
			return
		}
		f.ParentID = &id
	}
	tasks, err := s.Store.ListTasks(r.Context(), f)
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, tasks)
}

// TaskDetail is a task with everything the CLI's `show` prints.
type TaskDetail struct {
	store.Task
	Children []store.Task    `json:"children"`
	Comments []store.Comment `json:"comments"`
	Usage    []store.Usage   `json:"usage"`
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	t, err := s.Store.GetTask(ctx, id)
	if err != nil {
		failErr(w, err)
		return
	}
	d := TaskDetail{Task: *t}
	if d.Children, err = s.Store.ListTasks(ctx, store.ListFilter{ParentID: &id}); err == nil {
		if d.Comments, err = s.Store.Comments(ctx, id); err == nil {
			d.Usage, err = s.Store.UsageTotals(ctx, id)
		}
	}
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, d)
}

func (s *Server) updateTask(w http.ResponseWriter, r *http.Request) {
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var u store.Update
	if !decode(w, r, &u) {
		return
	}
	t, err := s.Store.UpdateTask(r.Context(), id, u)
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, t)
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request) {
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var c struct {
		Author string `json:"author"`
		Body   string `json:"body"`
	}
	if !decode(w, r, &c) {
		return
	}
	out, err := s.Store.AddComment(r.Context(), id, c.Author, c.Body)
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusCreated, out)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	ev, err := s.Store.Events(r.Context(), id)
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, ev)
}

func (s *Server) recordUsage(w http.ResponseWriter, r *http.Request) {
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var u struct {
		store.Usage
		Actor string `json:"actor"`
	}
	if !decode(w, r, &u) {
		return
	}
	u.TaskID = id
	if err := s.Store.RecordUsage(r.Context(), u.Usage, u.Actor); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusCreated, u.Usage)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		fail(w, http.StatusBadRequest, "q is required")
		return
	}
	hits, err := s.Store.SearchComments(r.Context(), q)
	if err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, hits)
}

type column struct {
	State string
	Tasks []store.Task
}

// board renders one column per state, in lifecycle order.
func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.Store.ListTasks(r.Context(), store.ListFilter{})
	if err != nil {
		failErr(w, err)
		return
	}
	cols := make([]column, len(store.States))
	index := map[string]int{}
	for i, st := range store.States {
		cols[i].State = st
		index[st] = i
	}
	for _, t := range tasks {
		if i, ok := index[t.State]; ok {
			cols[i].Tasks = append(cols[i].Tasks, t)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := boardPage.Execute(w, cols); err != nil {
		log.Printf("board page: %v", err)
	}
}
