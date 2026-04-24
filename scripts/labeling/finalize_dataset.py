#!/usr/bin/env python3
"""Combine seeds + synthetic into a deduped, split OPF training dataset.

Inputs:
  --seeds       train-seeds*.jsonl (from compile_trainset.py; can repeat)
  --synthetic   train-synth*.jsonl (from synthesize.py; can repeat)

Actions:
  1. Concatenate all inputs.
  2. Deduplicate — same `text` twice is redundant; keep the first occurrence.
  3. Cap per-span-text negatives to avoid the dataset being 50k copies of
     "/Users/andrew.sulistio/..." (regex-only negatives explode this class).
  4. Shuffle with a seed for reproducibility.
  5. Split 80/10/10 into train/val/test.
  6. Emit three JSONL files + a stats.json.

Usage:
    uv run python scripts/labeling/finalize_dataset.py \
        --seeds /tmp/train-seeds.jsonl --seeds /tmp/train-seeds-all.jsonl \
        --synthetic /tmp/train-synth-full.jsonl \
        --out-dir /tmp/opf-trainset/
"""
from __future__ import annotations

import argparse
import collections
import json
import random
from pathlib import Path


def _load_jsonl(paths: list[Path]) -> list[dict]:
    examples: list[dict] = []
    for p in paths:
        if not p.exists():
            continue
        with p.open("r") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    obj = json.loads(line)
                    if "text" in obj and "spans" in obj:
                        examples.append(obj)
                except json.JSONDecodeError:
                    continue
    return examples


def _validate(ex: dict) -> bool:
    text = ex.get("text")
    spans = ex.get("spans")
    if not isinstance(text, str) or not isinstance(spans, list):
        return False
    for sp in spans:
        s = sp.get("start"); e = sp.get("end"); lbl = sp.get("label")
        if not (isinstance(s, int) and isinstance(e, int) and isinstance(lbl, str)):
            return False
        if not (0 <= s < e <= len(text)):
            return False
    return True


def _dedupe(examples: list[dict]) -> list[dict]:
    seen: set[str] = set()
    out: list[dict] = []
    for ex in examples:
        key = ex["text"]
        if key in seen:
            continue
        seen.add(key)
        out.append(ex)
    return out


def _cap_negatives_by_span(examples: list[dict], cap: int) -> list[dict]:
    """Negative examples have spans=[], so we can't dedupe by span_text directly.
    Instead, cap by 'longest non-space token' in the text — the thing a detector
    would have flagged. This keeps the home_path negatives capped and diverse."""
    out: list[dict] = []
    neg_counter: collections.Counter = collections.Counter()
    for ex in examples:
        if ex["spans"]:
            out.append(ex)
            continue
        tokens = ex["text"].split()
        flagged = max(tokens, key=len) if tokens else ""
        # Bucket by first 20 chars of the longest token
        bucket = flagged[:20]
        if neg_counter[bucket] >= cap:
            continue
        neg_counter[bucket] += 1
        out.append(ex)
    return out


def _split(examples: list[dict], ratios: tuple[float, float, float], seed: int):
    rng = random.Random(seed)
    shuffled = examples[:]
    rng.shuffle(shuffled)
    n = len(shuffled)
    n_train = int(n * ratios[0])
    n_val = int(n * ratios[1])
    train = shuffled[:n_train]
    val = shuffled[n_train:n_train + n_val]
    test = shuffled[n_train + n_val:]
    return train, val, test


def _stats(examples: list[dict]) -> dict:
    by_label: collections.Counter = collections.Counter()
    n_pos = 0
    n_neg = 0
    for ex in examples:
        if ex["spans"]:
            n_pos += 1
            for sp in ex["spans"]:
                by_label[sp["label"]] += 1
        else:
            n_neg += 1
    return {
        "total": len(examples),
        "positive": n_pos,
        "negative": n_neg,
        "by_label": dict(by_label),
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--seeds", action="append", type=Path, default=[])
    ap.add_argument("--synthetic", action="append", type=Path, default=[])
    ap.add_argument("--out-dir", type=Path, required=True)
    ap.add_argument("--negative-cap-per-bucket", type=int, default=50,
                    help="Cap duplicate-ish negatives at N per span-text bucket.")
    ap.add_argument("--train-ratio", type=float, default=0.8)
    ap.add_argument("--val-ratio", type=float, default=0.1)
    ap.add_argument("--seed", type=int, default=42)
    args = ap.parse_args()

    all_inputs = list(args.seeds) + list(args.synthetic)
    if not all_inputs:
        print("Need at least --seeds or --synthetic", flush=True)
        return 1

    raw = _load_jsonl(all_inputs)
    valid = [ex for ex in raw if _validate(ex)]
    print(f"Loaded {len(raw)} raw, {len(valid)} valid ({len(raw) - len(valid)} dropped)")

    deduped = _dedupe(valid)
    print(f"After text-dedup: {len(deduped)}")

    capped = _cap_negatives_by_span(deduped, args.negative_cap_per_bucket)
    print(f"After negative-bucket cap @{args.negative_cap_per_bucket}: {len(capped)}")

    train_ratio = args.train_ratio
    val_ratio = args.val_ratio
    test_ratio = max(0.0, 1.0 - train_ratio - val_ratio)
    train, val, test = _split(capped, (train_ratio, val_ratio, test_ratio), args.seed)

    args.out_dir.mkdir(parents=True, exist_ok=True)
    for name, ex_list in [("train", train), ("val", val), ("test", test)]:
        path = args.out_dir / f"{name}.jsonl"
        with path.open("w") as f:
            for ex in ex_list:
                f.write(json.dumps(ex, ensure_ascii=False) + "\n")
        print(f"  {name}: {len(ex_list)} → {path}")

    stats = {
        "train": _stats(train),
        "val": _stats(val),
        "test": _stats(test),
        "config": {
            "seeds": [str(p) for p in args.seeds],
            "synthetic": [str(p) for p in args.synthetic],
            "negative_cap_per_bucket": args.negative_cap_per_bucket,
            "ratios": [train_ratio, val_ratio, test_ratio],
            "shuffle_seed": args.seed,
        },
    }
    stats_path = args.out_dir / "stats.json"
    with stats_path.open("w") as f:
        json.dump(stats, f, indent=2, sort_keys=True)
    print(f"  stats: {stats_path}")
    print()
    print(f"train positive/negative: {stats['train']['positive']}/{stats['train']['negative']}")
    print(f"train by label: {stats['train']['by_label']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
