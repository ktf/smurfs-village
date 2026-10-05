#!/bin/sh
# Start the board: restore the database if needed, then serve it under Litestream.
#
# The order matters. Restoring BEFORE serving, and stopping on any restore
# error, is what keeps an unreachable S3 from starting an empty board that would
# then replicate over the good copy. Nomad retries the task later instead.
set -eu

: "${NOMAD_ALLOC_DIR:?}" "${NOMAD_TASK_DIR:?}" "${NOMAD_PORT_http:?}"

# alloc/data lives on the group's ephemeral disk, which is sticky and migrated:
# a restart on the same node (or a move with a successful migration) finds the
# database already here, and restore skips straight to serving.
export BOARD_DB="$NOMAD_ALLOC_DIR/data/board.db"
mkdir -p "$(dirname "$BOARD_DB")"

# S3 goes through the security-proxy sidecar: its s3 route holds the bucket's
# real keys and re-signs each request. Wait until the sidecar is up and
# provisioned; it answers with the route's port once it is.
until port=$(board proxy-port -route s3 2>/dev/null); do
  echo "waiting for the security-proxy sidecar"
  sleep 5
done
export S3_ENDPOINT="http://127.0.0.1:$port"

# The AWS SDK asks `board s3-creds` for credentials and again before they
# expire (one hour), so the daily gate-token rotation never strands Litestream.
export AWS_CONFIG_FILE="$NOMAD_TASK_DIR/aws-config"
cat > "$AWS_CONFIG_FILE" <<EOF
[default]
region = us-east-1
credential_process = /usr/local/bin/board s3-creds -route s3
EOF
export AWS_SDK_LOAD_CONFIG=1
# Plain bodies, no aws-chunked trailers: the proxy re-signs with UNSIGNED-PAYLOAD.
export AWS_REQUEST_CHECKSUM_CALCULATION=when_required
export AWS_RESPONSE_CHECKSUM_VALIDATION=when_required

# Everything that touches the database runs under the job's Nomad variable
# lock: a second allocation (say, a replacement started while this node is cut
# off) waits for it, and losing it stops Litestream and the service at once.
# Two writers on one S3 replica would corrupt it.
exec board lock -- sh -ec '
  litestream restore -config /etc/litestream.yml -if-db-not-exists -if-replica-exists "$BOARD_DB"
  exec litestream replicate -config /etc/litestream.yml \
    -exec "board serve -db $BOARD_DB -listen 0.0.0.0:$NOMAD_PORT_http"
'
