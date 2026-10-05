// Package nomadlock holds a Nomad variable lock for as long as a child process
// runs, so that at most one copy of the board writes its database.
//
// It talks to the local Nomad agent through the Task API socket
// ($NOMAD_SECRETS_DIR/api.sock) with the task's workload identity (NOMAD_TOKEN,
// from `identity { env = true }`), so the job handles no other token.
//
// Timing: the lock has a TTL and is renewed every TTL/3. A renewal that fails
// (for instance on a node cut off from the servers) stops the child at once,
// well before the lock can expire and, after its lock delay, be taken by a
// replacement allocation.
package nomadlock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ErrHeld is returned by Acquire when another holder has the lock.
var ErrHeld = errors.New("lock held by someone else")

type Client struct {
	HTTP      *http.Client
	Base      string // e.g. http://localhost for the Task API socket
	Token     string
	Namespace string
}

// TaskAPI returns a client for the Task API unix socket.
func TaskAPI(socket, token, namespace string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Client{HTTP: &http.Client{Transport: tr, Timeout: 5 * time.Second},
		Base: "http://localhost", Token: token, Namespace: namespace}
}

type lockSpec struct {
	TTL       string `json:",omitempty"`
	LockDelay string `json:",omitempty"`
	ID        string `json:",omitempty"`
}

type variable struct {
	Namespace string `json:",omitempty"`
	Path      string
	Items     map[string]string `json:",omitempty"`
	Lock      *lockSpec         `json:",omitempty"`
}

func (c *Client) put(ctx context.Context, op string, v variable) (*variable, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/v1/var/%s?%s", c.Base, url.PathEscape(v.Path), op)
	if c.Namespace != "" {
		u += "&namespace=" + url.QueryEscape(c.Namespace)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Nomad-Token", c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusConflict {
		return nil, ErrHeld
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", op, v.Path, resp.StatusCode, bytes.TrimSpace(data))
	}
	var out variable
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s %s: %w", op, v.Path, err)
	}
	return &out, nil
}

// Acquire takes the lock once, returning its ID, or ErrHeld.
func (c *Client) Acquire(ctx context.Context, path, holder string, ttl, delay time.Duration) (string, error) {
	v, err := c.put(ctx, "lock-acquire", variable{Namespace: c.Namespace, Path: path,
		Items: map[string]string{"holder": holder},
		Lock:  &lockSpec{TTL: ttl.String(), LockDelay: delay.String()}})
	if err != nil {
		return "", err
	}
	if v.Lock == nil || v.Lock.ID == "" {
		return "", fmt.Errorf("lock-acquire %s: no lock ID in the reply", path)
	}
	return v.Lock.ID, nil
}

func (c *Client) Renew(ctx context.Context, path, id string) error {
	_, err := c.put(ctx, "lock-renew", variable{Namespace: c.Namespace, Path: path, Lock: &lockSpec{ID: id}})
	return err
}

func (c *Client) Release(ctx context.Context, path, id string) error {
	_, err := c.put(ctx, "lock-release", variable{Namespace: c.Namespace, Path: path, Lock: &lockSpec{ID: id}})
	return err
}

// Hold renews the lock until ctx ends (returns nil) or a renewal fails
// (returns the error: the caller must stop doing whatever the lock protects).
func (c *Client) Hold(ctx context.Context, path, id string, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, every)
			err := c.Renew(rctx, path, id)
			cancel()
			if err != nil && ctx.Err() == nil {
				return fmt.Errorf("lost the lock: %w", err)
			}
		}
	}
}
