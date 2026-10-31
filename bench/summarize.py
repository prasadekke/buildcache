#!/usr/bin/env python3
"""bench/summarize.py -- turn raw run.sh CSV output into a summary table.

Usage: summarize.py bench/results/<label>.csv [more.csv ...]

Prints a markdown table (mean +/- population stddev wall time per
scenario, plus mean hit rate where applicable) to stdout. Never edits the
raw CSVs -- this is a read-only view over them, so the underlying
per-run data is always available to recompute or audit independently.
"""
import csv
import statistics
import sys
from collections import defaultdict


def load(paths):
    rows = []
    for path in paths:
        with open(path, newline="") as f:
            rows.extend(csv.DictReader(f))
    return rows


def main():
    if len(sys.argv) < 2:
        print(__doc__, file=sys.stderr)
        sys.exit(1)

    rows = load(sys.argv[1:])
    if not rows:
        print("no rows found", file=sys.stderr)
        sys.exit(1)

    by_key = defaultdict(list)
    for r in rows:
        by_key[(r["label"], r["scenario"])].append(r)

    print("| repo label | scenario | trials | mean wall (s) | stddev (s) | mean hit rate |")
    print("|---|---|---|---|---|---|")

    for (label, scenario), group in sorted(by_key.items()):
        times = [float(r["wall_seconds"]) for r in group]
        mean = statistics.mean(times)
        stdev = statistics.pstdev(times) if len(times) > 1 else 0.0

        hit_rates = [float(r["hit_rate_pct"]) for r in group if r["hit_rate_pct"] not in ("", "0")]
        hit_rate_str = f"{statistics.mean(hit_rates):.1f}%" if hit_rates else "n/a"

        print(f"| {label} | {scenario} | {len(times)} | {mean:.2f} | {stdev:.2f} | {hit_rate_str} |")

    # One repo's commit, printed once per label for the report's methodology section.
    seen_labels = set()
    print("\nPinned commits:", file=sys.stderr)
    for r in rows:
        if r["label"] not in seen_labels:
            seen_labels.add(r["label"])
            print(f"  {r['label']}: {r['repo']} @ {r['commit']}", file=sys.stderr)


if __name__ == "__main__":
    main()
