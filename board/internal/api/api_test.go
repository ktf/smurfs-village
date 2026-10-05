package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"pi2/board/internal/store"
)

func server(t *testing.T, token string) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer((&Server{Store: st, Token: token}).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func call(t *testing.T, ts *httptest.Server, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}

func TestTaskRoundTrip(t *testing.T) {
	ts := server(t, "")
	code, body := call(t, ts, "POST", "/api/tasks", "", map[string]any{"title": "bump fmt", "created_by": "ktf", "repo": "alidist"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var task store.Task
	json.Unmarshal(body, &task)

	if code, body = call(t, ts, "PATCH", "/api/tasks/1", "", map[string]any{"state": "done", "actor": "x"}); code != http.StatusConflict {
		t.Fatalf("queued -> done: %d %s, want 409", code, body)
	}
	if code, body = call(t, ts, "PATCH", "/api/tasks/1", "", map[string]any{"assignee": "glm-1", "state": "working", "actor": "glm-1"}); code != http.StatusOK {
		t.Fatalf("claim: %d %s", code, body)
	}
	if code, body = call(t, ts, "POST", "/api/tasks/1/comments", "", map[string]any{"author": "glm-1", "body": "bumped to 11.2.0"}); code != http.StatusCreated {
		t.Fatalf("comment: %d %s", code, body)
	}
	if code, body = call(t, ts, "POST", "/api/tasks/1/usage", "", map[string]any{"runtime": "pi", "backend": "glm", "model": "GLM-5.3-Flash", "output": 40, "actor": "glm-1"}); code != http.StatusCreated {
		t.Fatalf("usage: %d %s", code, body)
	}
	code, body = call(t, ts, "GET", "/api/tasks/1", "", nil)
	var d TaskDetail
	json.Unmarshal(body, &d)
	if code != http.StatusOK || d.State != "working" || len(d.Comments) != 1 || len(d.Usage) != 1 {
		t.Fatalf("show: %d %s", code, body)
	}
	if code, _ = call(t, ts, "GET", "/api/tasks/99", "", nil); code != http.StatusNotFound {
		t.Fatalf("missing task: %d, want 404", code)
	}
	if code, _ = call(t, ts, "POST", "/api/tasks", "", map[string]any{"title": "x", "created_by": "k", "colour": "red"}); code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d, want 400", code)
	}
}

func TestTokenAndBoardPage(t *testing.T) {
	ts := server(t, "s3cret")
	if code, _ := call(t, ts, "GET", "/api/tasks", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", code)
	}
	if code, _ := call(t, ts, "POST", "/api/tasks", "s3cret", map[string]any{"title": "<b>escape me</b>", "created_by": "ktf"}); code != http.StatusCreated {
		t.Fatalf("with token: %d", code)
	}
	code, page := call(t, ts, "GET", "/", "", nil)
	if code != http.StatusOK || !strings.Contains(string(page), "Needs human · 0") ||
		!strings.Contains(string(page), "&lt;b&gt;escape me&lt;/b&gt;") {
		t.Fatalf("board page: %d\n%s", code, page)
	}
}
