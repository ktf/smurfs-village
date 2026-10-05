# Lets the board task take its single-writer lock (smurf lock, Nomad variable
# locks). A workload identity can only read its job's variables by default.
# Applied once by an admin, bound to the task's identity:
#
#   nomad acl policy apply -namespace default -job smurf-board -group board -task board \
#     smurf-board-lock deploy/smurf-board-lock.policy.hcl
namespace "default" {
  variables {
    path "nomad/jobs/smurf-board/leader" {
      capabilities = ["read", "write"]
    }
  }
}
