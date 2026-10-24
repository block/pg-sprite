#!/usr/bin/env python3
"""Check that a replay manifest tiles its corpus.

Usage: coverage.py MANIFEST CORPUS_DIR BASELINE FILE...

Every substantive line of every non-baseline corpus file must be claimed by
exactly one manifest range, in order. Blank and comment-only lines may sit
between ranges; the baseline is excluded because the harness applies it
wholesale. Exits 1 and prints each gap to stderr when the manifest does not
tile the corpus.
"""

import os
import sys

manifest, corpus, baseline = sys.argv[1], sys.argv[2], sys.argv[3]
files = sys.argv[4:]

ok = True


def bad(msg):
    global ok
    ok = False
    print(f"coverage: {msg}", file=sys.stderr)


rows = []  # (prefix, start, end) in manifest order
for raw in open(manifest):
    line = raw.strip()
    if not line or line.startswith("#"):
        continue
    prefix, rng = line.split()[:2]
    start, end = (int(n) for n in rng.split("-"))
    rows.append((prefix, start, end))

by_prefix = {f.split("_", 1)[0]: f for f in files}

claimed = {}  # file -> [(start, end)] in manifest order
for prefix, start, end in rows:
    f = by_prefix.get(prefix)
    if f is None:
        bad(f"row {prefix} {start}-{end} matches no corpus file")
        continue
    if f == baseline:
        bad(f"row {prefix} {start}-{end} claims the baseline {f}, which the harness applies wholesale")
        continue
    claimed.setdefault(f, []).append((start, end))

order = [prefix for prefix, _, _ in rows]
if order != sorted(order):
    bad("manifest rows are not in corpus order")

for f in files:
    if f == baseline:
        continue
    lines = open(os.path.join(corpus, f)).read().splitlines()
    covered = set()
    last_end = 0
    for start, end in claimed.get(f, []):
        if start > end:
            bad(f"{f}: inverted range {start}-{end}")
        if start <= last_end:
            bad(f"{f}: range {start}-{end} overlaps or precedes an earlier range (last end {last_end})")
        if end > len(lines):
            bad(f"{f}: range {start}-{end} runs past the end of the file ({len(lines)} lines)")
        covered.update(range(start, end + 1))
        last_end = max(last_end, end)
    for n, text in enumerate(lines, 1):
        t = text.strip()
        if not t or t.startswith("--"):
            continue
        if n not in covered:
            bad(f"{f}: line {n} is substantive but no manifest row claims it: {t[:60]}")

sys.exit(0 if ok else 1)
