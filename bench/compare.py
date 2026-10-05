#!/usr/bin/env python3
"""Merge the results of several runs into one table.

    ./compare.py runs/20261001-133746 runs/20261001-164500
"""
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from run import summary  # noqa: E402

results = [json.loads(line) for d in sys.argv[1:] for line in open(Path(d) / "results.jsonl")]
runners = list(dict.fromkeys(r["runner"] for r in results))
print(summary(results, runners))
