# Lets the board task take its single-writer lock (board lock, Nomad variable
# locks). A workload identity can only read its job's variables by default.
# Applied once by an admin, bound to the task's identity:
#
#   nomad acl policy apply -namespace default -job agent-board -group board -task board \
#     agent-board-lock deploy/board-lock.policy.hcl
namespace "default" {
  variables {
    path "nomad/jobs/agent-board/leader" {
      capabilities = ["read", "write"]
    }
  }
}
