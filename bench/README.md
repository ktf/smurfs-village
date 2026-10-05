# Agent benchmark: self-hosted models × agent loops, against Claude

Ten small aliBuild and C++ tasks, each with a hidden check. Runners are a model
plus an agent loop (Claude Code or pi); self-hosted models are reached through
the security proxy:

| Backend | Proxy route | Model | Context | Runners |
|---|---|---|---|---|
| `glm` | `glm` (pcapiserv12, llama.cpp) | GLM-5.3-Flash | 131k | `claude-glm`, `pi-glm` |
| `qwen38` | `aigw` (CERN LiteLLM gateway) | qwen3.8-27b-fp16 | 131k | `claude-qwen38`, `pi-qwen38` |
| `gptoss` | `aigw` | gpt-oss-20b | 32k | `pi-gptoss` only: Claude Code overflows 32k after a few file reads |
| `qwen3` | `aigw` | hf-qwen3-32b-awq | 16k | `pi-qwen3` only: Claude Code's first request (~18k tokens) does not fit |

Plus `claude-sonnet` and `claude-opus` on the subscription as the reference.
Backends live in the `BACKENDS` table in `run.py`.

```sh
./run.py --validate               # harness self-check, no agents (~10 s)
./run.py                          # 10 tasks x {claude-glm, pi-glm}, sequential
./run.py -t rename -r pi-glm      # a subset
./run.py -r claude-sonnet         # subscription baseline (uses your quota)
./run.py --load 1,2,4,8 -r pi-glm # throughput with N agents at once on the GLM server
./costs.py runs/A runs/B --hw-eur 70000 --watts 2000 --duty 0.5 --speedup <from --load>
```

`--load` runs the easy and medium tasks (2 rounds each, `--rounds`) through a pool
of N agents for every N, and reports tasks/hour, the throughput gain over one
agent, and how much each task slows down. Results land in `load.md`. The gain is
what `costs.py --speedup` needs to turn the one-at-a-time payback into the real
one. Throughput is jobs over makespan, so the last wave, where fewer than N
agents are still busy, pulls the higher levels down a little; raise `--rounds`
for a tighter number. The server must run llama.cpp with `--parallel N` (or
more slots) for N agents to actually overlap.

Results land in `runs/<stamp>/`: `summary.md`, `results.jsonl`, and per run
`<runner>/<task>/{stream.jsonl,stderr.txt,check.txt,diff.patch,ws/}`.

## Pieces

- `base/alibuild/` — tracked files of `~/src/alibuild` at the revision in
  `base/alibuild.rev`, as a git repo. Every run gets a fresh copy.
- `base/fixtures/` — the alidist `fmt.sh` recipe and two small C++ programs.
- `checks/` — hidden checks the agents never see.
- `run.py` — tasks (prompt, broken setup, check, reference fix) and runners.
- `pi-agent/` — pi's config dir for these runs (`models.json` is rewritten each
  run, because the proxy port is random). `node_modules/` has pi itself.
- `claude-config/` — an empty `CLAUDE_CONFIG_DIR`, so Claude Code runs without
  your CLAUDE.md, skills, memory or hooks, like pi with `--no-context-files
  --no-skills --no-extensions`.

## Fairness notes

- Both loops get the same prompt, shell, read/edit/write tools and the same
  15-minute timeout; Claude Code is capped at 60 turns, pi has no turn cap.
- Agents run unsandboxed as you, inside `runs/…/ws`. The tasks are benign, but
  the workspace is not a security boundary.
- `--validate` checks every task both ways: the broken setup fails its check and
  a reference fix passes it.
