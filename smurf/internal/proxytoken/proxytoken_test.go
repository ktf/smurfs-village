package proxytoken

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeProxy answers each connection the way the sidecar's agent socket does.
func fakeProxy(t *testing.T, reply func(route string) string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pt") // short path: unix socket names are limited to ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			route, _ := bufio.NewReader(c).ReadString('\n')
			c.Write([]byte(reply(strings.TrimSpace(route)) + "\n"))
			c.Close()
		}
	}()
	return sock
}

func TestAsk(t *testing.T) {
	sock := fakeProxy(t, func(route string) string {
		if route != "s3" {
			return `{"error": "unknown service", "services": ["s3"]}`
		}
		return `{"port": 41234, "token": "gate-abc"}`
	})
	r, err := Ask(sock, "s3")
	if err != nil || r.Port != 41234 || r.Token != "gate-abc" {
		t.Fatalf("Ask: %+v %v", r, err)
	}
	if _, err := Ask(sock, "github"); err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("unknown route: %v", err)
	}
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	c := S3Credentials(r, now)
	if c.Version != 1 || c.AccessKeyID != "gate-abc" || c.Expiration != "2026-10-02T10:00:00Z" {
		t.Fatalf("credentials: %+v", c)
	}
}

func TestAskNoProxy(t *testing.T) {
	if _, err := Ask(filepath.Join(t.TempDir(), "missing.sock"), "s3"); err == nil {
		t.Fatal("no socket: want an error")
	}
}
