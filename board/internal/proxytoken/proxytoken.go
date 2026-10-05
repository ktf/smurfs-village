// Package proxytoken asks a security-proxy sidecar for its port and a route's
// rotating gate token, over the proxy's agent socket.
//
// The protocol is one line each way: send "<route>\n", read back one JSON line
// with "port" and "token". The gate token rotates daily (the previous one stays
// valid one more window), so long-running clients must ask again rather than
// cache it; CredentialProcess exists for exactly that.
package proxytoken

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

type Reply struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	Error string `json:"error"`
}

func Ask(socket, route string) (*Reply, error) {
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "%s\n", route); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, fmt.Errorf("no reply from %s: %w", socket, err)
	}
	var r Reply
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, fmt.Errorf("bad reply from %s: %w", socket, err)
	}
	if r.Error != "" {
		return nil, fmt.Errorf("proxy: %s", r.Error)
	}
	if r.Token == "" || r.Port == 0 {
		return nil, fmt.Errorf("proxy gave no token or port for route %q", route)
	}
	return &r, nil
}

// CredentialProcess is the JSON an AWS SDK expects from a credential_process
// helper. The proxy validates the access key id (the gate token) and re-signs
// with the real keys, so the secret is a placeholder. Expiration makes the SDK
// call the helper again well before the token rotates.
type CredentialProcess struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Expiration      string `json:"Expiration"`
}

func S3Credentials(r *Reply, now time.Time) CredentialProcess {
	return CredentialProcess{
		Version:         1,
		AccessKeyID:     r.Token,
		SecretAccessKey: "signed-by-the-proxy",
		Expiration:      now.Add(time.Hour).UTC().Format(time.RFC3339),
	}
}
