package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func str(s string) *string { return &s }

func mustCreate(t *testing.T, s *Store, n NewTask) *Task {
	t.Helper()
	if n.CreatedBy == "" {
		n.CreatedBy = "ktf"
	}
	task, err := s.CreateTask(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func move(t *testing.T, s *Store, id int64, u Update) *Task {
	t.Helper()
	if u.Actor == "" {
		u.Actor = "test"
	}
	task, err := s.UpdateTask(context.Background(), id, u)
	if err != nil {
		t.Fatalf("task %d: %v", id, err)
	}
	return task
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	task := mustCreate(t, s, NewTask{Title: "fix asList"})
	if task.State != Queued {
		t.Fatalf("new task is %s, want queued", task.State)
	}

	if _, err := s.UpdateTask(ctx, task.ID, Update{State: str(Working), Actor: "w1"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("working without an assignee: got %v, want ErrInvalid", err)
	}
	move(t, s, task.ID, Update{Assignee: str("w1"), State: str(Working)})
	if _, err := s.UpdateTask(ctx, task.ID, Update{State: str(Done), Actor: "w1"}); !errors.Is(err, ErrTransition) {
		t.Fatalf("working -> done: got %v, want ErrTransition", err)
	}
	move(t, s, task.ID, Update{State: str(Review), ResultBranch: str("agent/1")})
	got := move(t, s, task.ID, Update{State: str(Queued), Actor: "ktf"})
	if got.Assignee != "" {
		t.Fatalf("re-queued task still assigned to %q", got.Assignee)
	}

	events, err := s.Events(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"created", "assigned", "state", "state", "state"}
	if len(kinds) != len(want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
}

func TestBlockedParentResumes(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	parent := mustCreate(t, s, NewTask{Title: "port the reader"})
	move(t, s, parent.ID, Update{Assignee: str("opus"), State: str(Working)})
	a := mustCreate(t, s, NewTask{Title: "part a", ParentID: &parent.ID, CreatedBy: "opus"})
	b := mustCreate(t, s, NewTask{Title: "part b", ParentID: &parent.ID, CreatedBy: "opus"})
	move(t, s, parent.ID, Update{State: str(Blocked)})

	for _, c := range []*Task{a, b} {
		move(t, s, c.ID, Update{Assignee: str("glm"), State: str(Working)})
	}
	move(t, s, a.ID, Update{State: str(Review)})
	if p, _ := s.GetTask(ctx, parent.ID); p.State != Blocked {
		t.Fatalf("parent %s after one of two subtasks, want blocked", p.State)
	}
	move(t, s, b.ID, Update{State: str(Review)})
	p, _ := s.GetTask(ctx, parent.ID)
	if p.State != Queued || p.Assignee != "" {
		t.Fatalf("parent %s/%q after all subtasks, want queued and unassigned", p.State, p.Assignee)
	}
}

func TestDelegationLimits(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	id := mustCreate(t, s, NewTask{Title: "level 1"}).ID
	for depth := 2; depth <= MaxDepth; depth++ {
		parent := id
		id = mustCreate(t, s, NewTask{Title: "deeper", ParentID: &parent}).ID
	}
	if _, err := s.CreateTask(ctx, NewTask{Title: "too deep", ParentID: &id, CreatedBy: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("depth %d: got %v, want ErrInvalid", MaxDepth+1, err)
	}

	root := mustCreate(t, s, NewTask{Title: "fan out"}).ID
	for i := 0; i < MaxChildren; i++ {
		mustCreate(t, s, NewTask{Title: "child", ParentID: &root})
	}
	if _, err := s.CreateTask(ctx, NewTask{Title: "one too many", ParentID: &root, CreatedBy: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("child %d: got %v, want ErrInvalid", MaxChildren+1, err)
	}
	missing := int64(9999)
	if _, err := s.CreateTask(ctx, NewTask{Title: "orphan", ParentID: &missing, CreatedBy: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing parent: got %v, want ErrNotFound", err)
	}
}

func TestCommentsSearchAndUsage(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	task := mustCreate(t, s, NewTask{Title: "segfault in DPL"})
	if _, err := s.AddComment(ctx, task.ID, "glm-1", "AddressSanitizer reports a heap-use-after-free in the reader"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddComment(ctx, task.ID, "ktf", "try a smaller timeframe"); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchComments(ctx, "heap")
	if err != nil || len(hits) != 1 || hits[0].Author != "glm-1" {
		t.Fatalf("search heap: %v %+v", err, hits)
	}
	if _, err := s.AddComment(ctx, 9999, "x", "nowhere"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("comment on a missing task: got %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := s.RecordUsage(ctx, Usage{TaskID: task.ID, Runtime: "pi", Backend: "glm", Model: "GLM-5.3-Flash",
			Input: 100, CacheRead: 1000, Output: 50, WallS: 10}, "glm-1"); err != nil {
			t.Fatal(err)
		}
	}
	totals, err := s.UsageTotals(ctx, task.ID)
	if err != nil || len(totals) != 1 || totals[0].CacheRead != 2000 || totals[0].WallS != 20 {
		t.Fatalf("usage totals: %v %+v", err, totals)
	}
}

func TestNullLabelsBecomeEmpty(t *testing.T) {
	s := open(t)
	task := mustCreate(t, s, NewTask{Title: "no labels", Labels: []byte("null")})
	if string(task.Labels) != "{}" {
		t.Fatalf("labels %s, want {}", task.Labels)
	}
}

func TestListOrder(t *testing.T) {
	s := open(t)
	mustCreate(t, s, NewTask{Title: "low"})
	mustCreate(t, s, NewTask{Title: "high", Priority: 5})
	mustCreate(t, s, NewTask{Title: "low too"})
	got, err := s.ListTasks(context.Background(), ListFilter{State: Queued})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Title != "high" || got[1].Title != "low" {
		t.Fatalf("order: %+v", got)
	}
}
