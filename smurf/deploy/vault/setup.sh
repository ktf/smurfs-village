#!/bin/sh
# One-time Vault setup for the smurf-board job, through the security proxy's
# attended vault-admin route (run `security-proxy-unlock vault-admin` first).
#
#   ./setup.sh            # show what would be written
#   ./setup.sh --apply    # write it
#
# 1. A KV v2 mount smurfs-village/, outside kv/, so the shared "nomad" role
#    (which reads kv/*) cannot see the board's secrets.
# 2. Policy smurf-board: read smurfs-village/data/board only.
# 3. Role smurf-board on Nomad's JWT auth mount: a copy of the existing "nomad"
#    role (audience, claim mappings, TTLs stay as the cluster has them), bound to
#    the smurf-board job and granting only the smurf-board policy.
#
# Then store the keypair (the secret never on screen or in argv):
#   ssh aiadm.cern.ch 'OS_PROJECT_NAME="ALICE Release testing" openstack ec2 credentials show <access-id> -f json' \
#     | jq '{s3_access_key: .access, s3_secret_key: .secret}' | vault-admin kv put smurfs-village/board -
set -eu
cd "$(dirname "$0")"
JWT_MOUNT=${JWT_MOUNT:-jwt-nomad}   # Nomad's Vault JWT auth mount; check with `vault-admin auth list`
JOB=smurf-board
SOCK=/usr/local/var/run/security-proxy/agent/agent.sock

fail() { echo "setup.sh: $*" >&2; exit 1; }

# Same as the vault-admin shell function: the admin route's gate token.
VAULT_ADDR=$(security-proxy-token --socket "$SOCK" --addr) || fail "security proxy not reachable"
VAULT_TOKEN=$(security-proxy-token --socket "$SOCK" vault-admin) || fail "no vault-admin route in the proxy"
export VAULT_ADDR VAULT_TOKEN

# Read the existing role first, as its own step: a failure here must stop the
# script, never turn into an empty role written below.
nomad_role=$(vault read -format=json "auth/$JWT_MOUNT/role/nomad") \
  || fail "cannot read auth/$JWT_MOUNT/role/nomad (vault-admin unlocked? right JWT_MOUNT?)"
role=$(printf '%s' "$nomad_role" | jq -e --arg job "$JOB" '
  .data
  | del(.policies)                                   # alias of token_policies; write one only
  | .bound_claims = ((.bound_claims // {}) + {nomad_namespace: "default", nomad_job_id: $job})
  | .token_policies = [$job]') || fail "unexpected role format"

echo "== role auth/$JWT_MOUNT/role/$JOB:"
echo "$role"
echo "== policy $JOB:"
cat smurf-board.policy.hcl

if [ "${1:-}" != "--apply" ]; then
  echo "(dry run; re-run with --apply to write)"
  exit 0
fi

mounts=$(vault secrets list -format=json) || fail "cannot list secrets engines"
if printf '%s' "$mounts" | jq -e '."smurfs-village/"' >/dev/null; then
  echo "mount smurfs-village/ already exists"
else
  vault secrets enable -path=smurfs-village -version=2 kv
fi
vault policy write "$JOB" smurf-board.policy.hcl
printf '%s' "$role" | vault write "auth/$JWT_MOUNT/role/$JOB" -
echo "done: mount smurfs-village/, policy and role $JOB"
