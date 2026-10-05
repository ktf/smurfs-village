package nomadlock

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNomad implements just enough of the variables lock API.
type fakeNomad struct {
	mu        sync.Mutex
	holder    string // lock ID, "" when free
	failRenew bool
	calls     []string
}

func (f *fakeNomad) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, "/v1/var/") || r.Header.Get("X-Nomad-Token") != "wi-token" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var v variable
	json.NewDecoder(r.Body).Decode(&v)
	op := r.URL.RawQuery
	if i := strings.IndexByte(op, '&'); i >= 0 {
		op = op[:i]
	}
	f.calls = append(f.calls, op)
	switch op {
	case "lock-acquire":
		if f.holder != "" {
			http.Error(w, "lock held", http.StatusConflict)
			return
		}
		f.holder = "lock-1"
		json.NewEncoder(w).Encode(variable{Path: v.Path, Lock: &lockSpec{ID: f.holder, TTL: v.Lock.TTL}})
	case "lock-renew", "lock-release":
		if f.failRenew || v.Lock == nil || v.Lock.ID != f.holder {
			http.Error(w, "not the holder", http.StatusConflict)
			return
		}
		if op == "lock-release" {
			f.holder = ""
		}
		json.NewEncoder(w).Encode(variable{Path: v.Path})
	default:
		http.Error(w, "unknown op", http.StatusBadRequest)
	}
}

func client(t *testing.T, f *fakeNomad) *Client {
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	return &Client{HTTP: ts.Client(), Base: ts.URL, Token: "wi-token", Namespace: "default"}
}

func TestAcquireRenewRelease(t *testing.T) {
	f := &fakeNomad{}
	c := client(t, f)
	ctx := context.Background()
	id, err := c.Acquire(ctx, "nomad/jobs/smurf-board/leader", "alloc-a", 15*time.Second, 15*time.Second)
	if err != nil || id != "lock-1" {
		t.Fatalf("acquire: %q %v", id, err)
	}
	if _, err := c.Acquire(ctx, "nomad/jobs/smurf-board/leader", "alloc-b", 15*time.Second, 15*time.Second); !errors.Is(err, ErrHeld) {
		t.Fatalf("second acquire: %v, want ErrHeld", err)
	}
	if err := c.Renew(ctx, "nomad/jobs/smurf-board/leader", id); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(ctx, "nomad/jobs/smurf-board/leader", id); err != nil {
		t.Fatal(err)
	}
	if f.holder != "" {
		t.Fatal("still held after release")
	}
}

func TestHoldReportsLoss(t *testing.T) {
	f := &fakeNomad{}
	c := client(t, f)
	id, _ := c.Acquire(context.Background(), "p", "a", 15*time.Second, 15*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Hold(ctx, "p", id, 20*time.Millisecond) }()
	time.Sleep(70 * time.Millisecond)
	f.mu.Lock()
	f.failRenew = true
	f.mu.Unlock()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "lost the lock") {
			t.Fatalf("Hold: %v, want a lost-lock error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Hold kept going after renewals started failing")
	}
}

func TestHoldStopsCleanly(t *testing.T) {
	f := &fakeNomad{}
	c := client(t, f)
	id, _ := c.Acquire(context.Background(), "p", "a", 15*time.Second, 15*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Hold(ctx, "p", id, 20*time.Millisecond) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Hold after cancel: %v", err)
	}
}
