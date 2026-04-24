#!/usr/bin/env python3
"""Compile reviewer decisions into an OPF training JSONL.

Input:  decisions.jsonl from review.py
Output: train.jsonl in OPF's expected format:
    {"text": "<context_before><span><context_after>",
     "spans": [{"start": N, "end": M, "label": "secret"}]}

Notes:
- "reject" decisions become training examples with zero spans — the model
  learns these look like secrets but aren't (negative training).
- "skip" decisions are filtered out.
- The text window is just the context ± span we already captured (~128 chars).
  OPF trains fine on short windows; for richer examples, expand context at
  mining time rather than post-hoc.
"""
from __future__ import annotations

import argparse
import json
import random
import sys
from pathlib import Path


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--decisions", type=Path, required=True)
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--shuffle-seed", type=int, default=42)
    ap.add_argument("--include-skipped", action="store_true",
                    help="Include skipped candidates as negatives (default: omit).")
    args = ap.parse_args()

    examples: list[dict] = []
    seen_ids: set[str] = set()
    with args.decisions.open("r") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            rec = json.loads(line)
            # Last-write-wins if the same candidate was reviewed more than once.
            if rec["id"] in seen_ids:
                # Overwrite the prior decision for this id.
                examples = [e for e in examples if e["_id"] != rec["id"]]
            seen_ids.add(rec["id"])

            decision = rec["decision"]
            if decision == "skip" and not args.include_skipped:
                continue

            before = rec.get("context_before") or ""
            span = rec.get("span_text") or ""
            after = rec.get("context_after") or ""

            # Strip the "…" truncation markers we added in extract/mine.
            before = before.lstrip("…")
            after = after.rstrip("…")
            # Unescape the "\\n" we used for display.
            before = before.replace("\\n", "\n")
            after = after.replace("\\n", "\n")

            text = before + span + after
            span_start = len(before)
            span_end = span_start + len(span)

            spans: list[dict] = []
            if decision in ("accept", "relabel"):
                label = rec.get("final_label")
                if label and label != "O":
                    spans.append({
                        "start": span_start, "end": span_end, "label": label,
                    })
            # "reject" → empty spans (negative example)

            examples.append({
                "_id": rec["id"],
                "text": text,
                "spans": spans,
            })

    random.Random(args.shuffle_seed).shuffle(examples)

    args.out.parent.mkdir(parents=True, exist_ok=True)
    with args.out.open("w") as out:
        for ex in examples:
            del ex["_id"]
            out.write(json.dumps(ex, ensure_ascii=False) + "\n")

    n_pos = sum(1 for ex in examples if ex["spans"])
    n_neg = len(examples) - n_pos
    print(f"Wrote {len(examples)} examples ({n_pos} positive, {n_neg} negative) "
          f"to {args.out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
