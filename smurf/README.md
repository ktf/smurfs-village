# smurf — the Smurfs Village tool

One static Go binary for the whole village: `smurf board` is the task board
(its service and its CLI), and `smurf lock`, `s3-creds` and `proxy-port` are
the helpers the Nomad jobs use. Design: the "Smurfs Village" doc; this is M0 of
its rollout plan.

```sh
go test ./...
CGO_ENABLED=0 go build -o bin/smurf ./cmd/smurf                                     # this Mac
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/smurf-linux-amd64 ./cmd/smurf  # Nomad

bin/smurf board serve -db board.db -listen 127.0.0.1:8080  # board page at http://127.0.0.1:8080/
export SMURF_URL=http://127.0.0.1:8080                      # + SMURF_TOKEN, SMURF_ACTOR (default $USER)
smurf add -repo O2 -p 2 "Port the AOD reader" "details..."
smurf ls [-state queued] [-parent 1] [-assignee glm-1]
smurf show 1
smurf mv 1 working -assignee opus-1         # move ID STATE [-assignee A] [-branch B]
smurf comment 1 "split into two readers"
smurf events 1
smurf search heap
```

The everyday commands are shortcuts: `smurf add` is `smurf board add`, and the
same for `ls`/`list`, `show`, `comment`, `mv`/`move`, `events` and `search`.
Scripts and job files use the full form.

## What the board enforces

- **States** `queued → working → review → done`, plus `blocked` (waiting for
  subtasks) and `needs_human`. Other moves get HTTP 409. `working` needs an
  assignee; going back to `queued` clears it.
- **Subtasks** (`-parent`) nest at most 3 deep, 10 per task. A `blocked` parent
  is re-queued automatically once all its subtasks reach `review` or `done`.
- **History**: every change is an event in the same transaction (`smurf events`).
- **Usage**: `POST /api/tasks/{id}/usage` with runtime, backend, model and the
  four token counts; `smurf show` prints the totals.

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

With `SMURF_TOKEN` set on the service, `/api/` requires `Authorization: Bearer <token>`.

## Deploying the board on the CI Nomad cluster (`deploy/`)

Job and Consul service `smurf-board`, one allocation, on any node. The database
lives on the group's sticky, migrated ephemeral disk and Litestream streams it
to `s3://smurfs-village/board`:

1. `entrypoint.sh` waits for the security-proxy sidecar, then runs everything
   under `smurf lock` (a Nomad variable lock): a second allocation waits, and an
   allocation that cannot renew the lock stops at once. Two writers on one S3
   replica would corrupt it.
2. `litestream restore -if-db-not-exists -if-replica-exists`: a reused local
   disk skips it; an empty bucket starts a fresh board; an unreachable S3 fails
   the task instead of starting empty.
3. `litestream replicate -exec "smurf board serve ..."`; on a stop Litestream
   ships the last writes (`kill_timeout = 30s`).

S3 credentials follow `queue-metrics.nomad`: the bootstrap sidecar pushes the
bucket's keypair from Vault into the proxy sidecar, and Litestream only ever
sees a rotating gate token, through the AWS SDK's `credential_process`
(`smurf s3-creds`), re-read every hour.

No image of its own while smurf is under heavy development: the board task
builds `smurf` at start-up in the stock Go image (pinned by digest) from
`github.com/ktf/smurfs-village` at the commit given as `smurf_ref`, and fetches
Litestream with its checksum. `deploy/entrypoint.sh` and `deploy/litestream.yml`
come from that same commit, so push before deploying:

```sh
sl push --to main
nomad-rw job run -var smurf_ref=$(sl log -r . -T '{node}') deploy/smurf-board.nomad.hcl
```

A branch name is refused at start-up: a restart must never run something nobody
deployed. Rolling back is running the older hash. Go's caches are on the sticky
disk, so a restart on the same node rebuilds in seconds. `deploy/Dockerfile` is
kept for when smurf settles into a `docks` image.

Before the first deployment:

- [x] A keypair of its own in the ALICE Release testing project (the bucket's
      project), created on aiadm.cern.ch. Its keys reach the whole project
      (alibuild-ac, alibuild-cas too); the proxy signs with them for
      `smurfs-village` only (`s3_scope_violation`).
- [x] Vault (admin, once): `deploy/vault/setup.sh --apply` created the
      `smurfs-village/` KV mount (outside `kv/`, which the shared `nomad` role
      reads), the `smurf-board` policy and a `smurf-board` role usable only by
      this job; the keypair is at `smurfs-village/board`.
- [ ] A security-proxy image with `s3_scope_violation` (docks `security-proxy`,
      `ALI_BOT_REF` = the ali-bot commit), pinned in the proxy and bootstrap tasks.
- [ ] Apply the lock policy once (admin): see `deploy/smurf-board-lock.policy.hcl`.

Checked so far: unit tests; the restore/replicate/restart flow with a local file
replica; `smurf lock` against a fake Task API (waits, hands over on SIGTERM,
stops on lost renewals); `nomad job validate` against the cluster; a clean
linux/amd64 build of the pushed commit with Go 1.27.1, the image's version.
Not yet checked: the build and Litestream inside the allocation, and S3 through
the proxy's signing.
