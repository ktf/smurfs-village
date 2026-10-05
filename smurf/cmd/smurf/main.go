// Command smurf is the Smurfs Village tool: the task board, the service behind
// it, and the helpers its Nomad jobs use.
//
//	smurf board serve [-db board.db] [-listen 127.0.0.1:8080]
//	smurf board add [-repo R] [-p N] [-parent ID] [-labels JSON] "title" [body]
//	smurf board list [-state S] [-parent ID] [-assignee A]
//	smurf board show ID
//	smurf board comment ID "text"
//	smurf board move ID STATE [-assignee A] [-branch B]
//	smurf board events ID
//	smurf board search QUERY
//
// The everyday board commands also work without "board": smurf add, ls, show,
// comment, mv, events, search.
//
//	smurf lock [-path P] [-ttl 15s] -- CMD ARGS...  run CMD only while holding a Nomad variable lock
//	smurf s3-creds [-socket S] [-route s3]          AWS credential_process helper
//	smurf proxy-port [-socket S] [-route s3]
//
// Board clients read SMURF_URL (default http://127.0.0.1:8080), SMURF_TOKEN and
// SMURF_ACTOR (default $USER): the name recorded on comments and changes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"smurfs-village/smurf/internal/api"
	"smurfs-village/smurf/internal/nomadlock"
	"smurfs-village/smurf/internal/proxytoken"
	"smurfs-village/smurf/internal/store"
)

// boardCommands are the subcommands of "smurf board"; the ones in shortcuts
// may also be given directly after "smurf".
var boardCommands = map[string]func([]string) error{
	"serve": serve, "add": add, "list": list, "ls": list, "show": show, "comment": comment,
	"move": move, "mv": move, "events": events, "search": search,
}

var shortcuts = map[string]bool{"add": true, "list": true, "ls": true, "show": true, "comment": true,
	"move": true, "mv": true, "events": true, "search": true}

var helpers = map[string]func([]string) error{"lock": lock, "s3-creds": s3Creds, "proxy-port": proxyPort}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	name := cmd
	run, ok := helpers[cmd]
	switch {
	case ok:
	case cmd == "board" && len(args) > 0 && boardCommands[args[0]] != nil:
		name = "board " + args[0]
		run, args = boardCommands[args[0]], args[1:]
	case shortcuts[cmd]:
		name = "board " + cmd
		run = boardCommands[cmd]
	default:
		usage()
	}
	if err := run(args); err != nil {
		log.Fatalf("smurf %s: %v", name, err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: smurf board serve|add|list|show|comment|move|events|search ...
       smurf add|ls|show|comment|mv|events|search ...   (shortcuts for smurf board ...)
       smurf lock|s3-creds|proxy-port ...                (helpers for the Nomad jobs)
see the package documentation for the flags`)
	os.Exit(2)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := fs.String("db", "board.db", "SQLite database path")
	listen := fs.String("listen", "127.0.0.1:8080", "address to serve on")
	fs.Parse(args)
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	srv := &http.Server{
		Addr:              *listen,
		Handler:           (&api.Server{Store: st, Token: os.Getenv("SMURF_TOKEN")}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("smurf board: serving %s on http://%s", *db, *listen)
	return srv.ListenAndServe()
}

// ---- client side

func actor() string {
	if a := os.Getenv("SMURF_ACTOR"); a != "" {
		return a
	}
	return os.Getenv("USER")
}

func request(method, path string, body, out any) error {
	base := os.Getenv("SMURF_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	var rd io.Reader
	if body != nil {
		js, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(js)
	}
	req, err := http.NewRequest(method, strings.TrimRight(base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("SMURF_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func taskArg(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("task id %q is not a number", s)
	}
	return id, nil
}

func add(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	repo := fs.String("repo", "", "repository the task is about")
	rev := fs.String("rev", "", "base revision")
	prio := fs.Int("p", 0, "priority (higher is picked first)")
	parent := fs.Int64("parent", 0, "parent task, for a subtask")
	labels := fs.String("labels", "", "labels as JSON")
	fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf(`usage: smurf board add [flags] "title" [body]`)
	}
	n := store.NewTask{Title: fs.Arg(0), Body: strings.Join(fs.Args()[1:], " "), Repo: *repo, BaseRev: *rev,
		Priority: *prio, CreatedBy: actor()}
	if *parent != 0 {
		n.ParentID = parent
	}
	if *labels != "" {
		n.Labels = json.RawMessage(*labels)
	}
	var t store.Task
	if err := request("POST", "/api/tasks", n, &t); err != nil {
		return err
	}
	fmt.Printf("#%d %s (%s)\n", t.ID, t.Title, t.State)
	return nil
}

func list(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	state := fs.String("state", "", "only this state")
	parent := fs.Int64("parent", 0, "only subtasks of this task")
	assignee := fs.String("assignee", "", "only tasks held by this worker")
	fs.Parse(args)
	q := url.Values{}
	if *state != "" {
		q.Set("state", *state)
	}
	if *parent != 0 {
		q.Set("parent", strconv.FormatInt(*parent, 10))
	}
	if *assignee != "" {
		q.Set("assignee", *assignee)
	}
	var tasks []store.Task
	if err := request("GET", "/api/tasks?"+q.Encode(), nil, &tasks); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tP\tASSIGNEE\tREPO\tTITLE")
	for _, t := range tasks {
		title := t.Title
		if t.ParentID != nil {
			title = fmt.Sprintf("%s  (sub of #%d)", title, *t.ParentID)
		}
		fmt.Fprintf(w, "#%d\t%s\t%d\t%s\t%s\t%s\n", t.ID, t.State, t.Priority, t.Assignee, t.Repo, title)
	}
	return w.Flush()
}

func show(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: smurf board show ID")
	}
	id, err := taskArg(args[0])
	if err != nil {
		return err
	}
	var d api.TaskDetail
	if err := request("GET", fmt.Sprintf("/api/tasks/%d", id), nil, &d); err != nil {
		return err
	}
	fmt.Printf("#%d %s\n", d.ID, d.Title)
	fmt.Printf("state %s · priority %d · created by %s at %s\n", d.State, d.Priority, d.CreatedBy, d.CreatedAt)
	for _, kv := range [][2]string{{"assignee", d.Assignee}, {"repo", d.Repo}, {"base", d.BaseRev}, {"branch", d.ResultBranch}} {
		if kv[1] != "" {
			fmt.Printf("%s %s\n", kv[0], kv[1])
		}
	}
	if d.ParentID != nil {
		fmt.Printf("subtask of #%d\n", *d.ParentID)
	}
	if string(d.Labels) != "{}" {
		fmt.Printf("labels %s\n", d.Labels)
	}
	if d.Body != "" {
		fmt.Printf("\n%s\n", d.Body)
	}
	if len(d.Children) > 0 {
		fmt.Println("\nsubtasks:")
		for _, c := range d.Children {
			fmt.Printf("  #%d %-11s %s\n", c.ID, c.State, c.Title)
		}
	}
	if len(d.Comments) > 0 {
		fmt.Println("\ncomments:")
		for _, c := range d.Comments {
			fmt.Printf("  [%s] %s: %s\n", c.CreatedAt, c.Author, c.Body)
		}
	}
	for _, u := range d.Usage {
		fmt.Printf("\nusage %s/%s/%s: in %d, cache read %d, cache write %d, out %d, %.0f s\n",
			u.Runtime, u.Backend, u.Model, u.Input, u.CacheRead, u.CacheWrite, u.Output, u.WallS)
	}
	return nil
}

func comment(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf(`usage: smurf board comment ID "text"`)
	}
	id, err := taskArg(args[0])
	if err != nil {
		return err
	}
	return request("POST", fmt.Sprintf("/api/tasks/%d/comments", id),
		map[string]string{"author": actor(), "body": strings.Join(args[1:], " ")}, nil)
}

func move(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: smurf board move ID STATE [-assignee A] [-branch B]")
	}
	id, err := taskArg(args[0])
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("move", flag.ExitOnError)
	assignee := fs.String("assignee", "", "worker taking the task")
	branch := fs.String("branch", "", "result branch")
	fs.Parse(args[2:])
	u := store.Update{State: &args[1], Actor: actor()}
	if *assignee != "" {
		u.Assignee = assignee
	}
	if *branch != "" {
		u.ResultBranch = branch
	}
	var t store.Task
	if err := request("PATCH", fmt.Sprintf("/api/tasks/%d", id), u, &t); err != nil {
		return err
	}
	fmt.Printf("#%d %s\n", t.ID, t.State)
	return nil
}

func events(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: smurf board events ID")
	}
	id, err := taskArg(args[0])
	if err != nil {
		return err
	}
	var ev []store.Event
	if err := request("GET", fmt.Sprintf("/api/tasks/%d/events", id), nil, &ev); err != nil {
		return err
	}
	for _, e := range ev {
		fmt.Printf("%s  %-8s %-10s %s\n", e.At, e.Kind, e.Actor, e.Data)
	}
	return nil
}

func search(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: smurf board search QUERY")
	}
	var hits []store.Comment
	if err := request("GET", "/api/search?q="+url.QueryEscape(strings.Join(args, " ")), nil, &hits); err != nil {
		return err
	}
	for _, c := range hits {
		fmt.Printf("#%d  %s: %s\n", c.TaskID, c.Author, c.Body)
	}
	return nil
}

// ---- security-proxy sidecar helpers, used by the Nomad job

func proxyFlags(name string, args []string) (string, string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	socket := fs.String("socket", os.ExpandEnv("${NOMAD_ALLOC_DIR}/security-proxy/agent/agent.sock"), "agent socket")
	route := fs.String("route", "s3", "proxy route")
	fs.Parse(args)
	return *socket, *route
}

// s3Creds prints credentials for an AWS SDK credential_process: the route's
// current gate token as the access key id, asked for afresh on every call.
func s3Creds(args []string) error {
	r, err := proxytoken.Ask(proxyFlags("s3-creds", args))
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(proxytoken.S3Credentials(r, time.Now()))
}

func proxyPort(args []string) error {
	r, err := proxytoken.Ask(proxyFlags("proxy-port", args))
	if err != nil {
		return err
	}
	fmt.Println(r.Port)
	return nil
}

// lock runs a command only while this allocation holds a Nomad variable lock,
// so two copies of the board can never write the same database. It exits with
// the command's status; if the lock is lost, it stops the command and fails.
func lock(args []string) error {
	fs := flag.NewFlagSet("lock", flag.ExitOnError)
	path := fs.String("path", "nomad/jobs/"+os.Getenv("NOMAD_JOB_ID")+"/leader", "variable to lock")
	ttl := fs.Duration("ttl", 15*time.Second, "lock TTL (10s to 24h)")
	grace := fs.Duration("grace", 10*time.Second, "time the command gets to stop before SIGKILL")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: smurf lock [flags] -- CMD ARGS...")
	}
	c := nomadlock.TaskAPI(os.ExpandEnv("${NOMAD_SECRETS_DIR}/api.sock"), os.Getenv("NOMAD_TOKEN"), os.Getenv("NOMAD_NAMESPACE"))
	holder := os.Getenv("NOMAD_ALLOC_ID")

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { <-sigs; stop() }()

	var id string
	for {
		var err error
		if id, err = c.Acquire(ctx, *path, holder, *ttl, *ttl); err == nil {
			break
		}
		if !errors.Is(err, nomadlock.ErrHeld) && ctx.Err() == nil {
			log.Printf("smurf lock: %v; retrying", err)
		} else if ctx.Err() == nil {
			log.Printf("smurf lock: %s is held by another allocation; waiting", *path)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped before acquiring %s", *path)
		case <-time.After(5 * time.Second):
		}
	}
	log.Printf("smurf lock: holding %s", *path)
	defer c.Release(context.Background(), *path, id)

	cmd := exec.Command(fs.Arg(0), fs.Args()[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	lost := make(chan error, 1)
	go func() { lost <- c.Hold(ctx, *path, id, *ttl/3) }()

	terminate := func() error {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-exited:
			return err
		case <-time.After(*grace):
			cmd.Process.Kill()
			return <-exited
		}
	}
	select {
	case err := <-exited:
		return err
	case <-ctx.Done(): // asked to stop: let the command shut down cleanly
		return terminate()
	case err := <-lost:
		if err == nil {
			return terminate()
		}
		terminate()
		return err
	}
}
