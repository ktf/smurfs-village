# What the smurf-board job may read from Vault: the board's S3 keypair, nothing else.
# Its bootstrap sidecar logs in with the smurf-board role (setup.sh), which only
# the smurf-board job's workload identity can use.
path "smurfs-village/data/board" {
  capabilities = ["read"]
}
