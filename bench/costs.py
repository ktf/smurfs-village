#!/usr/bin/env python3
"""Token breakdown and cost per runner, and when self-hosted hardware pays for itself.

    ./costs.py runs/20261001-133746 runs/20261001-164155
    ./costs.py runs/... --hw-eur 70000 --watts 2000 --eur-kwh 0.20 --duty 0.5 --speedup 3.1

Tokens are split into uncached input, cache writes (5 min / 1 h), cache reads
and output, and priced at Anthropic list prices. For the self-hosted runners the
saving per task is what the *same task* cost on Claude, not GLM's own tokens at
Claude prices: GLM needs several times more tokens for the same work, so pricing
its tokens would overstate what the hardware saves.
"""
import argparse
import json
from collections import defaultdict
from pathlib import Path

# $ per million tokens, Anthropic first-party list prices (claude-api skill, 2026-09-25).
PRICES = {
    "claude-sonnet-5-5": {"input": 2.00, "cache_write_5m": 2.50, "cache_write_1h": 4.00,
                          "cache_read": 0.20, "output": 10.00},
    "claude-opus-5-5": {"input": 4.00, "cache_write_5m": 5.00, "cache_write_1h": 8.00,
                        "cache_read": 0.20, "output": 20.00},
    "claude-haiku-4-5": {"input": 1.00, "cache_write_5m": 1.25, "cache_write_1h": 2.00,
                         "cache_read": 0.10, "output": 5.00},
}
KINDS = ["input", "cache_write_5m", "cache_write_1h", "cache_read", "output"]
PAID = {"claude-sonnet", "claude-opus"}  # every other runner is a self-hosted model


def claude_tokens(stream):
    """Per-model token breakdown from Claude Code's final `result` event."""
    out = defaultdict(lambda: dict.fromkeys(KINDS, 0))
    for line in stream.read_text(errors="replace").splitlines():
        if '"type":"result"' not in line:
            continue
        e = json.loads(line)
        u = e["usage"]
        cc = u.get("cache_creation") or {}
        model_usage = e.get("modelUsage") or {}
        # The 5m/1h split is only reported in aggregate; apportion it per model.
        written = u.get("cache_creation_input_tokens") or 0
        share_1h = (cc.get("ephemeral_1h_input_tokens", 0) / written) if written else 1.0
        for model, mu in model_usage.items():
            t = out[mu.get("canonicalModel", model)]
            t["input"] += mu.get("inputTokens", 0)
            t["output"] += mu.get("outputTokens", 0)
            t["cache_read"] += mu.get("cacheReadInputTokens", 0)
            w = mu.get("cacheCreationInputTokens", 0)
            t["cache_write_1h"] += round(w * share_1h)
            t["cache_write_5m"] += w - round(w * share_1h)
    return out


def pi_tokens(stream):
    out = defaultdict(lambda: dict.fromkeys(KINDS, 0))
    for line in stream.read_text(errors="replace").splitlines():
        if '"message_end"' not in line:
            continue
        m = json.loads(line)["message"]
        if m.get("role") != "assistant":
            continue
        u, t = m.get("usage", {}), out[m.get("model", "?")]
        t["input"] += u.get("input", 0)
        t["output"] += u.get("output", 0)
        t["cache_read"] += u.get("cacheRead", 0)
        t["cache_write_5m"] += u.get("cacheWrite", 0)
    return out


def price(model, t):
    p = PRICES.get(model)
    return None if p is None else sum(t[k] * p[k] for k in KINDS) / 1e6


def load(run_dirs):
    rows = []
    for d in map(Path, run_dirs):
        for line in open(d / "results.jsonl"):
            r = json.loads(line)
            stream = d / r["runner"] / r["task"] / "stream.jsonl"
            per_model = (pi_tokens if r["runner"].startswith("pi") else claude_tokens)(stream)
            tokens = dict.fromkeys(KINDS, 0)
            cost = 0.0
            for model, t in per_model.items():
                for k in KINDS:
                    tokens[k] += t[k]
                c = price(model, t)
                cost = None if c is None or cost is None else cost + c
            rows.append({**r, "tokens": tokens, "cost": cost})
    return rows


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("runs", nargs="+")
    ap.add_argument("--hw-eur", type=float, help="hardware purchase price")
    ap.add_argument("--watts", type=float, default=0, help="average power draw under load")
    ap.add_argument("--eur-kwh", type=float, default=0.20)
    ap.add_argument("--usd-eur", type=float, default=0.92, help="EUR per USD")
    ap.add_argument("--years", type=float, default=4, help="depreciation period")
    ap.add_argument("--duty", type=float, default=1.0,
                    help="fraction of the day the server is busy with agent work")
    ap.add_argument("--speedup", type=float, default=1.0,
                    help="throughput with several agents at once relative to one (run.py --load)")
    a = ap.parse_args()

    rows = load(a.runs)
    runners = list(dict.fromkeys(r["runner"] for r in rows))
    tasks = list(dict.fromkeys(r["task"] for r in rows))

    print("## Tokens per runner (all tasks)\n")
    print("| runner | uncached input | cache write 5m | cache write 1h | cache read | output | list price |")
    print("|---|---|---|---|---|---|---|")
    for rn in runners:
        rs = [r for r in rows if r["runner"] == rn]
        tot = {k: sum(r["tokens"][k] for r in rs) for k in KINDS}
        cost = sum(r["cost"] for r in rs) if all(r["cost"] is not None for r in rs) else None
        print(f"| {rn} | " + " | ".join(f"{tot[k]:,}" for k in KINDS)
              + f" | {'self-hosted' if cost is None else f'${cost:.2f}'} |")

    print("\n## Cost per task on Claude (what a self-hosted run avoids)\n")
    paid = [rn for rn in runners if rn in PAID]
    print("| task | " + " | ".join(paid) + " |")
    print("|---|" + "---|" * len(paid))
    by = {(r["task"], r["runner"]): r for r in rows}
    for t in tasks:
        print(f"| {t} | " + " | ".join(
            f"${by[(t, rn)]['cost']:.3f}" if (t, rn) in by else "—" for rn in paid) + " |")

    if not a.hw_eur:
        return
    print("\n## Amortisation\n")
    for hosted in [rn for rn in runners if rn not in PAID]:
        for ref in paid:
            pairs = [(by[(t, hosted)], by[(t, ref)]) for t in tasks
                     if (t, hosted) in by and (t, ref) in by and by[(t, hosted)]["passed"]]
            if not pairs:
                continue
            wall = sum(h["wall_s"] for h, _ in pairs)          # server-seconds per task set
            saved_usd = sum(c["cost"] for _, c in pairs)       # Claude cost of the same set
            sets_per_day = 86400 * a.duty * a.speedup / wall
            saved_eur_day = sets_per_day * saved_usd * a.usd_eur
            power_eur_day = a.watts / 1000 * 24 * a.duty * a.eur_kwh
            net = saved_eur_day - power_eur_day
            days = a.hw_eur / net if net > 0 else float("inf")
            print(f"- **{hosted} vs {ref}**" + (f" at {a.speedup}x throughput" if a.speedup != 1 else "")
                  + f": {len(pairs)} tasks take {wall:.0f} s on the server (one at a time) and "
                  f"would cost ${saved_usd:.2f} on {ref}. Running such work {a.duty:.0%} of the day: "
                  f"{sets_per_day:.0f} task sets/day, €{saved_eur_day:.2f}/day avoided, "
                  f"€{power_eur_day:.2f}/day power → net €{net:.2f}/day → "
                  + (f"pays back in **{days:.0f} days** ({days / 365:.1f} years"
                     + (", longer than the depreciation period" if days > a.years * 365 else "") + ")"
                     if net > 0 else "**never pays back** (power costs more than it saves)"))


if __name__ == "__main__":
    main()
