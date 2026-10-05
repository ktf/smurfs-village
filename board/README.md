# board — the Agent Board service (M0)

One static Go binary: `board serve` runs the service, every other subcommand is
the CLI. Design: the "Agent Board" doc (M0 of its rollout plan).

```sh
go test ./...
CGO_ENABLED=0 go build -o bin/board ./cmd/board                                  # this Mac
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/board-linux-amd64 ./cmd/board  # Nomad

bin/board serve -db board.db -listen 127.0.0.1:8080   # board page at http://127.0.0.1:8080/
export BOARD_URL=http://127.0.0.1:8080                 # + BOARD_TOKEN, BOARD_ACTOR (default $USER)
bin/board add -repo O2 -p 2 "Port the AOD reader" "details..."
bin/board list [-state queued] [-parent 1] [-assignee glm-1]
bin/board show 1
bin/board move 1 working -assignee opus-1     # move ID STATE [-assignee A] [-branch B]
bin/board comment 1 "split into two readers"
bin/board events 1
bin/board search heap
```

## What it enforces

- **States** `queued → working → review → done`, plus `blocked` (waiting for
  subtasks) and `needs_human`. Other moves get HTTP 409. `working` needs an
  assignee; going back to `queued` clears it.
- **Subtasks** (`-parent`) nest at most 3 deep, 10 per task. A `blocked` parent
  is re-queued automatically once all its subtasks reach `review` or `done`.
- **History**: every change is an event in the same transaction (`board events`).
- **Usage**: `POST /api/tasks/{id}/usage` with runtime, backend, model and the
  four token counts; `board show` prints the totals.

## API

| Method | Path | |
|---|---|---|
| POST | `/api/tasks` | create (`title`, `created_by`, optional `body`, `repo`, `base_rev`, `priority`, `labels`, `parent_id`) |
| GET | `/api/tasks?state=&parent=&assignee=` | list, pick-up order (priority, then age) |
| GET | `/api/tasks/{id}` | task + subtasks + comments + usage totals |
| PATCH | `/api/tasks/{id}` | `state`, `assignee`, `priority`, `labels`, `result_branch`; `actor` required |
| POST | `/api/tasks/{id}/comments` | `author`, `body` |
| GET | `/api/tasks/{id}/events` | history |
| POST | `/api/tasks/{id}/usage` | token and time usage |
| GET | `/api/search?q=` | FTS5 search over comments |
| GET | `/` | read-only board page, refreshes every 15 s |

With `BOARD_TOKEN` set, `/api/` requires `Authorization: Bearer <token>`.

## Deploying on the CI Nomad cluster (`deploy/`)

One allocation, on any node. The database lives on the group's sticky,
migrated ephemeral disk and Litestream streams it to `s3://smurfs-village/agent-board`:

1. `entrypoint.sh` waits for the security-proxy sidecar, then runs everything
   under `board lock` (a Nomad variable lock): a second allocation waits, and an
   allocation that cannot renew the lock stops at once. Two writers on one S3
   replica would corrupt it.
2. `litestream restore -if-db-not-exists -if-replica-exists`: a reused local
   disk skips it; an empty bucket starts a fresh board; an unreachable S3 fails
   the task instead of starting empty.
3. `litestream replicate -exec "board serve ..."`; on a stop Litestream ships
   the last writes (`kill_timeout = 30s`).

S3 credentials follow `queue-metrics.nomad`: the bootstrap sidecar pushes the
bucket's keypair from Vault into the proxy sidecar, and Litestream only ever
sees a rotating gate token, through the AWS SDK's `credential_process`
(`board s3-creds`), re-read every hour.

Before the first `nomad-rw job run deploy/board.nomad.hcl`:

- [ ] Vault path and field names of the smurfs-village keypair (the `TODO` in
      the bootstrap task; the current `kv/data/s3-services` keys get 403 on it).
- [ ] Build and push the image: `docker build -f deploy/Dockerfile -t registry.cern.ch/alisw/agent-board:0.1.0 .`,
      then pin it by digest in the jobspec.
- [ ] Apply the lock policy once (admin): see `deploy/board-lock.policy.hcl`.

Checked so far: unit tests; the restore/replicate/restart flow with a local file
replica; `board lock` against a fake Task API (waits, hands over on SIGTERM,
stops on lost renewals); `nomad job validate` against the cluster. Not yet
checked: Litestream through the proxy's S3 signing, and the image build.
