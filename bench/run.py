#!/usr/bin/env python3
"""Compare agent runtimes on the same backend: Claude Code vs pi, both on GLM.

Every (task, runner) pair gets a fresh copy of base/alibuild (a frozen snapshot
of ~/src/alibuild, see base/alibuild.rev) with the task's setup applied, runs
the agent headless in it, then runs a hidden check. Results go to
runs/<stamp>/results.jsonl and runs/<stamp>/summary.md; workspaces and raw
agent streams are kept next to them for inspection.

    ./run.py                          # all tasks, runners claude-glm and pi-glm
    ./run.py -t rename -t cpp-offbyone
    ./run.py -r claude-sonnet         # subscription baseline (uses your quota)
    ./run.py --validate               # check the harness itself, no agents
    ./run.py --load 1,2,4,8 -r pi-glm # throughput with N agents at once on one server
"""
import argparse
import functools
import json
import os
import re
import shutil
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path

BENCH = Path(__file__).resolve().parent
BASE = BENCH / "base" / "alibuild"
FIXTURES = BENCH / "base" / "fixtures"
CHECKS = BENCH / "checks"
PY = os.path.expanduser("~/src/alibuild-devel/bin/python")  # has aliBuild's deps
SOCK = "/usr/local/var/run/security-proxy/agent/agent.sock"
GLM_MODEL = "GLM-5.3-Flash"

# Self-hosted backends: the security-proxy route that reaches each model, and
# its context window as the server reports it (probed 2026-10-05), which pi
# uses to decide when to compact. Claude Code's first request alone is about
# 18k tokens, so a small window runs with pi only ("claude": False): 16k cannot
# hold it at all, and at 32k the first file reads overflow it (the gateway then
# answers 500 and Claude Code retries for minutes before giving up).
BACKENDS = {
    "glm":    {"route": "glm",  "model": GLM_MODEL,          "reasoning": True,  "context": 131072, "claude": True},
    "qwen38": {"route": "aigw", "model": "qwen3.8-27b-fp16", "reasoning": False, "context": 131072, "claude": True},
    "gptoss": {"route": "aigw", "model": "gpt-oss-20b",      "reasoning": True,  "context": 32768,  "claude": False},
    "qwen3":  {"route": "aigw", "model": "hf-qwen3-32b-awq", "reasoning": False, "context": 16384,  "claude": False},
}
TIMEOUT = 15 * 60

SUFFIX = ("\n\nWork in the current directory. Do not ask questions: finish the task "
          "on your own, then reply with a one-line summary.")


def sh(cmd, cwd, timeout=300):
    p = subprocess.run(cmd, cwd=cwd, shell=True, capture_output=True, text=True, timeout=timeout)
    return p.returncode, p.stdout + p.stderr


# ---------------------------------------------------------------- tasks

def replace(ws, path, old, new):
    f = ws / path
    text = f.read_text()
    assert old in text, f"setup: {old!r} not found in {path}"
    f.write_text(text.replace(old, new, 1))


def drop_test(ws, path, name):
    """Remove one test method (up to the next def/class at the same indent)."""
    f = ws / path
    text = f.read_text()
    new = re.sub(rf"\n(\s+)def {name}\(.*?(?=\n\1def |\nclass |\Z)", "\n", text, count=1, flags=re.S)
    assert new != text, f"setup: test {name} not found"
    f.write_text(new)


@dataclass
class Task:
    name: str
    level: str
    prompt: str
    check: str  # shell, run in the workspace; exit 0 = pass
    setup: callable = None
    extra: list = field(default_factory=list)  # fixtures copied into the workspace
    solve: object = ""  # reference fix (shell or callable(ws)), used only by --validate


MAX_LOAD_ANCHOR = '  build_parser.add_argument("-e", dest="environment"'
UNIT = f"{PY} -m unittest tests.test_utilities tests.test_args"
ASAN = "clang++ -std=c++20 -g -fsanitize=address,undefined -fno-sanitize-recover=all"

TASKS = [
    Task("lookup-jobs", "easy",
         "What is the default value of the --jobs option of `aliBuild build`, and how is it "
         "computed? Write a one-line answer into ANSWER.txt.",
         r"grep -Eiq 'cpu_count|cpus?|cores|processors' ANSWER.txt",
         solve="echo 'multiprocessing.cpu_count(), the number of CPUs' > ANSWER.txt"),
    Task("lookup-devel-link", "easy",
         "In this aliBuild source tree, which file and which function create the "
         "BUILD/<package>-latest-<devel prefix> symlink for development packages? Write the "
         "answer into ANSWER.txt as `path/to/file.py:function_name`.",
         "grep -q 'alibuild_helpers/build.py' ANSWER.txt && grep -q 'doBuild' ANSWER.txt",
         solve="echo alibuild_helpers/build.py:doBuild > ANSWER.txt"),
    Task("recipe-bump", "easy",
         "Bump the fmt package in alidist/fmt.sh to version 11.2.0. Change nothing else.",
         "grep -q '^tag: \"11.2.0\"' alidist/fmt.sh && "
         "test \"$(git diff --numstat -- alidist/fmt.sh | cut -f1,2)\" = \"$(printf '1\\t1')\"",
         extra=[("fmt.sh", "alidist/fmt.sh")],
         solve="sed -i '' 's/^tag: \"11.1.2\"/tag: \"11.2.0\"/' alidist/fmt.sh"),
    Task("cpp-offbyone", "easy",
         "cpp/stats.cpp should print the mean of {1,2,3,4} but its result is wrong. Fix the bug.",
         f"{ASAN} cpp/stats.cpp -o /tmp/bench-stats-$$ && test \"$(/tmp/bench-stats-$$)\" = 2.50",
         extra=[("stats.cpp", "cpp/stats.cpp")],
         solve="sed -i '' 's/i <= v.size()/i < v.size()/' cpp/stats.cpp"),
    Task("fix-failing-test", "medium",
         f"`{UNIT}` fails. Find the bug in the code (not in the tests) and fix it.",
         f"{UNIT} && git diff --quiet -- tests",
         setup=lambda ws: replace(ws, "alibuild_helpers/utilities.py",
                                  "asList = lambda x : x if type(x) == list else [x]",
                                  "asList = lambda x : x if type(x) == list else x"),
         solve="git checkout HEAD~1 -- alibuild_helpers/utilities.py"),
    Task("rename", "medium",
         "Rename the function short_commit_hash to shorten_commit_hash everywhere in this "
         "code base, including all callers and imports.",
         "! grep -rn --include='*.py' short_commit_hash . && "
         f"{PY} -c 'import alibuild_helpers.build, alibuild_helpers.workarea, alibuild_helpers.sandbox; "
         "from alibuild_helpers.utilities import shorten_commit_hash' && " + UNIT,
         solve="grep -rl --include='*.py' short_commit_hash . | xargs sed -i '' 's/short_commit_hash/shorten_commit_hash/g'"),
    Task("feature-aslist", "medium",
         "Make alibuild_helpers.utilities.asList also turn tuples into lists (asList((1, 2)) "
         "== [1, 2]); other behaviour must not change. Add a unit test for it in "
         "tests/test_utilities.py.",
         f"{PY} -c 'from alibuild_helpers.utilities import asList as a; "
         "assert a((1,2))==[1,2] and a([1])==[1] and a(3)==[3] and a(\"x\")==[\"x\"]' && "
         f"{UNIT} && git diff -- tests/test_utilities.py | grep -q '^+.*asList'",
         solve=("sed -i '' 's/asList = lambda x : x if type(x) == list else \\[x\\]/"
                "asList = lambda x : x if type(x) == list else list(x) if type(x) == tuple else [x]/' "
                "alibuild_helpers/utilities.py && sed -i '' '/def test_asList/a\\\n"
                "    self.assertEqual(asList((1, 2)), [1, 2])\n' tests/test_utilities.py")),
    Task("cli-option", "medium",
         "Add a `--max-load N` option to the `build` subcommand of aliBuild (float, stored "
         "as args.maxLoad, default None) with a sensible help text. Only the option is "
         "needed, not its use. Add a test for it in tests/test_args.py.",
         f"{PY} {CHECKS}/check_max_load.py && {UNIT} && "
         "git diff -- tests/test_args.py | grep -q '^+.*max-load'",
         solve=lambda ws: (
             replace(ws, "alibuild_helpers/args.py", MAX_LOAD_ANCHOR,
                     '  build_parser.add_argument("--max-load", dest="maxLoad", type=float, '
                     'default=None, help="x")\n' + MAX_LOAD_ANCHOR),
             open(ws / "tests/test_args.py", "a").write("# max-load\n"))),
    Task("diamond-symptom", "hard",
         "Users report that aliBuild sometimes builds a package before all of its dependencies "
         "have been built. It seems to happen when a package depends on several packages "
         "that themselves depend on each other. Find the root cause, fix it, and add a "
         "regression test.",
         f"{PY} {CHECKS}/test_topo_hidden.py && {UNIT}",
         setup=lambda ws: (
             replace(ws, "alibuild_helpers/utilities.py",
                     "leaves.extend(new_leaves - {pkg for pkg, _ in edges})",
                     "leaves.extend(new_leaves)"),
             drop_test(ws, "tests/test_utilities.py", "test_diamond_dependency"),
             drop_test(ws, "tests/test_utilities.py", "test_resolve_dependency_chain"),
             drop_test(ws, "tests/test_utilities.py", "test_dont_drop_packages")),
         solve="git checkout HEAD~1 -- alibuild_helpers/utilities.py"),
    Task("cpp-memory", "hard",
         "cpp/registry.cpp should print 4950 followed by DET0, DET1 and DET2, one per line, "
         "but it prints garbage or crashes depending on the platform. Find and fix all the "
         "memory bugs, keeping the overall design.",
         f"{ASAN} cpp/registry.cpp -o /tmp/bench-reg-$$ && "
         "test \"$(/tmp/bench-reg-$$)\" = \"$(printf '4950\\nDET0\\nDET1\\nDET2')\"",
         extra=[("registry.cpp", "cpp/registry.cpp")],
         solve=f"cp {FIXTURES}/registry.fixed.cpp cpp/registry.cpp"),
]


def make_workspace(task, ws):
    shutil.copytree(BASE, ws, symlinks=True,
                    ignore=shutil.ignore_patterns("fsmonitor--daemon*", "__pycache__"))
    for src, dst in task.extra:
        (ws / dst).parent.mkdir(parents=True, exist_ok=True)
        shutil.copy(FIXTURES / src, ws / dst)
    if task.setup:
        task.setup(ws)
    # Commit the broken state so the agent sees a clean tree and checks can diff.
    sh("git add -A && git -c user.name=bench -c user.email=bench@localhost "
       "commit -qm setup --allow-empty", ws)


# ---------------------------------------------------------------- runners

def proxy(*args):
    return subprocess.check_output(["security-proxy-token", "--socket", SOCK, *args], text=True).strip()


def claude_cmd(prompt, model=None):
    cmd = ["claude", "-p", prompt, "--output-format", "stream-json", "--verbose",
           "--setting-sources", "", "--strict-mcp-config", "--disable-slash-commands",
           "--no-session-persistence", "--permission-mode", "acceptEdits",
           "--allowedTools", "Bash Read Edit Write Glob Grep", "--max-turns", "60"]
    return cmd + (["--model", model] if model else [])


def run_claude_hosted(backend, prompt, ws):
    """Claude Code against a self-hosted model, through the proxy route of its backend."""
    route, model = BACKENDS[backend]["route"], BACKENDS[backend]["model"]
    cfg = BENCH / "claude-config"  # empty: no user CLAUDE.md, skills, memory or hooks
    cfg.mkdir(exist_ok=True)
    env = {k: v for k, v in os.environ.items() if k != "ANTHROPIC_API_KEY"}
    env.update(CLAUDE_CONFIG_DIR=str(cfg), ANTHROPIC_BASE_URL=proxy("--addr") + "/" + route,
               ANTHROPIC_AUTH_TOKEN=proxy(route), ANTHROPIC_MODEL=model,
               ANTHROPIC_DEFAULT_OPUS_MODEL=model, ANTHROPIC_DEFAULT_SONNET_MODEL=model,
               ANTHROPIC_DEFAULT_HAIKU_MODEL=model, CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1")
    return claude_cmd(prompt), env


def run_claude_sonnet(prompt, ws):
    return claude_cmd(prompt, "sonnet"), dict(os.environ)


def run_claude_opus(prompt, ws):
    return claude_cmd(prompt, "opus"), dict(os.environ)


@functools.cache  # once per invocation: parallel agents must not rewrite it under each other
def write_pi_models():
    """One pi provider per proxy route, holding every benchmarked model behind it."""
    agent = BENCH / "pi-agent"
    agent.mkdir(exist_ok=True)
    providers = {}
    for b in BACKENDS.values():
        p = providers.setdefault(b["route"], {
            "baseUrl": proxy("--addr") + "/" + b["route"],  # the proxy port is random: resolve per run
            "api": "anthropic-messages",
            "apiKey": f"!security-proxy-token --socket {SOCK} {b['route']}",
            "authHeader": True,
            "models": []})
        p["models"].append({"id": b["model"], "name": b["model"], "reasoning": b["reasoning"],
                            "contextWindow": b["context"], "maxTokens": 8192})
    (agent / "models.json").write_text(json.dumps({"providers": providers}, indent=2))
    return agent


def run_pi_hosted(backend, prompt, ws):
    """pi against a self-hosted model, through the proxy route of its backend."""
    env = dict(os.environ, PI_CODING_AGENT_DIR=str(write_pi_models()), PI_OFFLINE="1",
               PI_SKIP_VERSION_CHECK="1", PI_TELEMETRY="0")
    cmd = [str(BENCH / "node_modules/.bin/pi"), "-p", "--mode", "json", "--no-session",
           "--no-extensions", "--no-skills", "--no-context-files",
           "--provider", BACKENDS[backend]["route"], "--model", BACKENDS[backend]["model"], prompt]
    return cmd, env


def run_dry(prompt, ws):
    """No agent at all: exercises the harness itself, e.g. --load scheduling."""
    return ["sleep", "2"], dict(os.environ)


RUNNERS = {"claude-sonnet": run_claude_sonnet, "claude-opus": run_claude_opus, "dry": run_dry}
for _name, _b in BACKENDS.items():  # claude-glm, pi-glm, claude-qwen38, pi-qwen38, ...
    if _b["claude"]:
        RUNNERS[f"claude-{_name}"] = functools.partial(run_claude_hosted, _name)
    RUNNERS[f"pi-{_name}"] = functools.partial(run_pi_hosted, _name)


def parse_stream(runner, path):
    """Turns, tokens and tool calls from the agent's JSONL stream."""
    s = {"turns": 0, "tool_calls": 0, "input_tokens": 0, "output_tokens": 0,
         "first_input_tokens": None, "error": None, "cost_usd": None}
    for line in path.read_text(errors="replace").splitlines():
        try:
            e = json.loads(line)
        except json.JSONDecodeError:
            continue
        if runner.startswith("claude"):
            if e.get("type") == "assistant":
                u = e["message"].get("usage", {})
                if s["first_input_tokens"] is None:
                    s["first_input_tokens"] = (u.get("input_tokens", 0) + u.get("cache_read_input_tokens", 0)
                                               + u.get("cache_creation_input_tokens", 0))
                s["tool_calls"] += sum(c.get("type") == "tool_use" for c in e["message"]["content"])
            elif e.get("type") == "result":
                u = e.get("usage", {})
                s["turns"] = e.get("num_turns", 0)
                s["input_tokens"] = (u.get("input_tokens", 0) + u.get("cache_read_input_tokens", 0)
                                     + u.get("cache_creation_input_tokens", 0))
                s["output_tokens"] = u.get("output_tokens", 0)
                if runner in ("claude-sonnet", "claude-opus"):
                    # API-equivalent, also on a subscription. For a self-hosted model
                    # Claude Code prices tokens as if a Claude model had served them.
                    s["cost_usd"] = e.get("total_cost_usd")
                if e.get("is_error"):
                    s["error"] = str(e.get("subtype"))
        elif e.get("type") == "message_end" and e["message"].get("role") == "assistant":
            m = e["message"]
            u = m.get("usage", {})
            s["turns"] += 1
            s["tool_calls"] += sum(c.get("type") == "toolCall" for c in m.get("content", []))
            inp = u.get("input", 0) + u.get("cacheRead", 0) + u.get("cacheWrite", 0)
            if s["first_input_tokens"] is None:
                s["first_input_tokens"] = inp
            s["input_tokens"] += inp
            s["output_tokens"] += u.get("output", 0)
            if m.get("stopReason") == "error":
                s["error"] = (m.get("errorMessage") or "error")[:200]
    return s


def run_one(task, runner, outdir, tag=""):
    d = outdir / runner / (task.name + tag)
    ws = d / "ws"
    d.mkdir(parents=True)
    make_workspace(task, ws)
    cmd, env = RUNNERS[runner](task.prompt + SUFFIX, ws)
    t0 = time.time()
    timed_out = False
    with open(d / "stream.jsonl", "w") as out, open(d / "stderr.txt", "w") as err:
        try:
            subprocess.run(cmd, cwd=ws, env=env, stdout=out, stderr=err,
                           stdin=subprocess.DEVNULL, timeout=TIMEOUT)
        except subprocess.TimeoutExpired:
            timed_out = True
    wall = time.time() - t0
    rc, check_out = sh(task.check, ws)
    (d / "check.txt").write_text(check_out)
    sh("git diff HEAD > ../diff.patch; git status --short > ../status.txt", ws)
    return {"task": task.name, "level": task.level, "runner": runner, "passed": rc == 0,
            "wall_s": round(wall, 1), "timed_out": timed_out, **parse_stream(runner, d / "stream.jsonl")}


# ---------------------------------------------------------------- report

def summary(results, runners):
    lines = ["| task | level | " + " | ".join(runners) + " |",
             "|---|---|" + "---|" * len(runners)]
    for name in dict.fromkeys(r["task"] for r in results):
        row = {r["runner"]: r for r in results if r["task"] == name}
        any_r = next(iter(row.values()))
        cells = []
        for rn in runners:
            r = row.get(rn)
            cells.append("—" if r is None else
                         f"{'PASS' if r['passed'] else 'fail'} {r['wall_s']:.0f}s "
                         f"{r['turns']}t {r['input_tokens'] // 1000}k"
                         + (" TIMEOUT" if r["timed_out"] else ""))
        lines.append(f"| {name} | {any_r['level']} | " + " | ".join(cells) + " |")
    lines += ["", "| runner | passed | total wall | median wall | input tokens | first-request tokens | API cost |",
              "|---|---|---|---|---|---|---|"]
    for rn in runners:
        rs = [r for r in results if r["runner"] == rn]
        if not rs:
            continue
        walls = sorted(r["wall_s"] for r in rs)
        first = [r["first_input_tokens"] for r in rs if r["first_input_tokens"]]
        lines.append(f"| {rn} | {sum(r['passed'] for r in rs)}/{len(rs)} | {sum(walls):.0f}s | "
                     f"{walls[len(walls) // 2]:.0f}s | {sum(r['input_tokens'] for r in rs) // 1000}k | "
                     f"{(sum(first) // len(first)) if first else '—'} | "
                     + (f"${sum(r['cost_usd'] or 0 for r in rs):.2f}" if any(r.get("cost_usd") for r in rs) else "—")
                     + " |")
    lines.append("\nCell: result, wall time, assistant turns, input tokens (incl. cached).")
    return "\n".join(lines)


def run_load(tasks, runners, levels, rounds, outdir):
    """Throughput of one server with N agents at once.

    Every level runs the same jobs (tasks x rounds) through a pool of N workers.
    Throughput is jobs over makespan, so it includes the tail where fewer than N
    agents are still running; more rounds make that tail matter less.
    """
    rows = []
    for runner in runners:
        base = None
        for n in levels:
            jobs = [(t, i) for i in range(rounds) for t in tasks]
            t0 = time.time()
            with ThreadPoolExecutor(max_workers=n) as pool:
                results = list(pool.map(
                    lambda job: run_one(job[0], runner, outdir / f"x{n}", f"-{job[1]}"), jobs))
            makespan = time.time() - t0
            for r in results:
                r["concurrency"] = n
            with open(outdir / "results.jsonl", "a") as f:
                f.writelines(json.dumps(r) + "\n" for r in results)
            walls = sorted(r["wall_s"] for r in results)
            row = {"runner": runner, "concurrency": n, "jobs": len(jobs),
                   "passed": sum(r["passed"] for r in results), "makespan_s": round(makespan),
                   "tasks_per_hour": round(len(jobs) * 3600 / makespan, 1),
                   "median_task_s": walls[len(walls) // 2],
                   "output_tokens": sum(r["output_tokens"] for r in results)}
            base = base or row
            row["throughput_x"] = round(row["tasks_per_hour"] / base["tasks_per_hour"], 2)
            row["task_slowdown_x"] = round(row["median_task_s"] / base["median_task_s"], 2)
            rows.append(row)
            with open(outdir / "load.jsonl", "a") as f:
                f.write(json.dumps(row) + "\n")
            print(f"{runner:11} x{n:<2} {row['passed']}/{row['jobs']} passed  makespan {makespan:5.0f}s  "
                  f"{row['tasks_per_hour']:6.1f} tasks/h ({row['throughput_x']}x)  "
                  f"median task {row['median_task_s']:.0f}s ({row['task_slowdown_x']}x)", flush=True)
    return rows


def load_summary(rows):
    lines = ["| runner | agents at once | passed | makespan | tasks/hour | throughput | median task | task slowdown | output tok/s |",
             "|---|---|---|---|---|---|---|---|---|"]
    for r in rows:
        lines.append(f"| {r['runner']} | {r['concurrency']} | {r['passed']}/{r['jobs']} | {r['makespan_s']}s | "
                     f"{r['tasks_per_hour']} | {r['throughput_x']}x | {r['median_task_s']:.0f}s | "
                     f"{r['task_slowdown_x']}x | {r['output_tokens'] / r['makespan_s']:.0f} |")
    lines.append("\nThroughput feeds amortisation directly: ./costs.py ... --speedup <throughput>")
    return "\n".join(lines)


def validate():
    """The harness checks itself: every broken setup must fail its check."""
    ok = True
    tmp = BENCH / "runs" / "_validate"
    shutil.rmtree(tmp, ignore_errors=True)
    for task in TASKS:
        ws = tmp / task.name
        make_workspace(task, ws)
        rc, out = sh(task.check, ws)
        broken_fails = rc != 0
        if callable(task.solve):
            task.solve(ws)
            sol_out = ""
        else:
            _, sol_out = sh(task.solve, ws)
        rc2, out2 = sh(task.check, ws)
        fixed_passes = rc2 == 0
        ok &= broken_fails and fixed_passes
        print(f"  {'ok  ' if broken_fails else 'FAIL'} {task.name}: check fails before the fix"
              + ("" if broken_fails else f"\n{out[-500:]}"))
        print(f"  {'ok  ' if fixed_passes else 'FAIL'} {task.name}: check passes after the reference fix"
              + ("" if fixed_passes else f"\n{sol_out[-300:]}\n{out2[-800:]}"))
    shutil.rmtree(tmp, ignore_errors=True)
    return ok


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("-t", "--task", action="append", choices=[t.name for t in TASKS])
    ap.add_argument("-r", "--runner", action="append", choices=list(RUNNERS))
    ap.add_argument("--validate", action="store_true")
    ap.add_argument("--load", metavar="N,N,...",
                    help="parallel-load test: run the tasks with N agents at once, for each N")
    ap.add_argument("--rounds", type=int, default=2, help="--load: times each task runs per level")
    a = ap.parse_args()
    if a.validate:
        sys.exit(0 if validate() else 1)
    tasks = [t for t in TASKS if not a.task or t.name in a.task]
    runners = a.runner or ["claude-glm", "pi-glm"]
    outdir = BENCH / "runs" / time.strftime("%Y%m%d-%H%M%S")
    outdir.mkdir(parents=True)
    if a.load:
        if not a.task:  # one 15-minute hard task would dominate every level's makespan
            tasks = [t for t in tasks if t.level != "hard"]
        rows = run_load(tasks, runners, [int(n) for n in a.load.split(",")], a.rounds, outdir)
        (outdir / "load.md").write_text(load_summary(rows) + "\n")
        print("\n" + load_summary(rows) + f"\n\n{outdir}")
        return
    results = []
    for task in tasks:
        for runner in runners:  # interleaved, so server load drifts evenly over both
            r = run_one(task, runner, outdir)
            results.append(r)
            with open(outdir / "results.jsonl", "a") as f:
                f.write(json.dumps(r) + "\n")
            print(f"{task.name:18} {runner:13} {'PASS' if r['passed'] else 'fail'} "
                  f"{r['wall_s']:6.0f}s {r['turns']:3}t {r['input_tokens']:8} in"
                  + (f"  [{r['error']}]" if r["error"] else ""), flush=True)
    (outdir / "summary.md").write_text(summary(results, runners) + "\n")
    print("\n" + summary(results, runners) + f"\n\n{outdir}")


if __name__ == "__main__":
    main()
