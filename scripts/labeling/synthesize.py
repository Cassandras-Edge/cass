#!/usr/bin/env python3
"""Expand a reviewed training set with synthetic variants via Codex.

For each accepted positive example, asks Codex to generate N format-preserving
variants (same shape, different random content) embedded in M realistic prose
contexts. Emits the synthesized examples in the same OPF training JSONL shape
as `compile_trainset.py`, ready to concatenate with the human-reviewed set.

Usage:
    uv run python scripts/labeling/synthesize.py \
        --seeds /tmp/trainset.jsonl \
        --out /tmp/synthetic.jsonl \
        --variants 10 --contexts 3

Prereq: `cass` must be installed and `codex login` already done (the same
ChatGPT-backed path cass image uses). We shell out to `codex exec` rather than
calling the API directly, to reuse the existing auth.
"""
from __future__ import annotations

import argparse
import json
import subprocess
import sys
import threading
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path


PROMPT_POSITIVE = """You are expanding a training set for a PII/secret detection model.

Given this labeled example:
- Text: {text!r}
- Span: position [{start}, {end}] labeled {label!r}
- Extracted span text: {span_text!r}

Generate {n_variants} NEW training examples that:
1. Use the SAME format/shape as the span (e.g. if it's 'ghp_' + 36 hex chars, every variant must be 'ghp_' + 36 hex chars — just different random chars).
2. Wrap the span in {n_contexts} different realistic prose contexts per variant (total {total} examples).
3. Keep the span identifiable in the text with exact character offsets.

Format each example as a single JSON object, one per line, in this schema:
  {{"text": "<prose with span embedded>", "spans": [{{"start": N, "end": M, "label": {label!r}}}]}}

Only output the JSONL lines — no prose, no code fences, no commentary. Produce exactly {total} lines."""

PROMPT_NEGATIVE = """You are expanding a training set for a PII/secret detection model.

Given this NEGATIVE example (something that LOOKS like PII/secret but isn't):
- Text: {text!r}
- Visible string: {span_text!r}
- Why it's negative: it was flagged by the detector but a human marked it as not-PII

Generate {total} NEW negative training examples that use similar-looking strings in realistic prose contexts. The strings should be the kind of thing a regex or loose classifier might flag (long hex, path-like strings, high-entropy IDs) but are clearly NOT secrets or PII — e.g. git commit SHAs, session UUIDs, library version hashes, build artifact names, file paths.

Format each example as JSON, one per line:
  {{"text": "<realistic prose>", "spans": []}}

Note: negative examples have EMPTY spans arrays. Only output the JSONL lines — no prose, no code fences."""


def _call_codex(prompt: str, model: str, timeout: int = 180) -> str:
    """Invoke `codex exec` and return its stdout."""
    try:
        r = subprocess.run(
            ["codex", "exec", "--skip-git-repo-check", "--model", model, prompt],
            capture_output=True, text=True, timeout=timeout, check=False,
        )
    except FileNotFoundError:
        raise SystemExit("`codex` CLI not found. Install it and run `codex login`.")
    if r.returncode != 0:
        print(f"codex exec error (exit {r.returncode}): {r.stderr[-500:]}", file=sys.stderr)
        return ""
    return r.stdout


def _extract_jsonl(s: str) -> list[dict]:
    """Parse JSONL lines and dedup (codex emits streamed output then a final
    summary — the same lines appear twice in practice)."""
    out: list[dict] = []
    seen: set[str] = set()
    for line in s.splitlines():
        line = line.strip()
        if not line or not line.startswith("{"):
            continue
        if line in seen:
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(obj, dict) and "text" in obj and "spans" in obj:
            out.append(obj)
            seen.add(line)
    return out


def _validate_example(ex: dict) -> bool:
    """Reject examples where span offsets don't actually point at something."""
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


def _build_prompt(seed: dict, args) -> str:
    spans = seed.get("spans") or []
    if spans:
        sp = spans[0]
        total = args.variants * args.contexts
        return PROMPT_POSITIVE.format(
            text=seed["text"], start=sp["start"], end=sp["end"],
            label=sp["label"],
            span_text=seed["text"][sp["start"]:sp["end"]],
            n_variants=args.variants, n_contexts=args.contexts, total=total,
        )
    text = seed["text"]
    span_text = max(text.split(), key=len) if text.strip() else "abc123"
    total = args.negatives_per_seed
    return PROMPT_NEGATIVE.format(text=text, span_text=span_text, total=total)


def _process_seed(idx: int, total: int, seed: dict, args) -> tuple[int, list[dict], list[dict]]:
    """Generate synth examples for one seed. Returns (idx, valid_examples, rejected)."""
    prompt = _build_prompt(seed, args)
    raw = _call_codex(prompt, args.model)
    parsed = _extract_jsonl(raw)
    valid: list[dict] = []
    rejected: list[dict] = []
    for ex in parsed:
        if _validate_example(ex):
            valid.append(ex)
        else:
            rejected.append(ex)
    return idx, valid, rejected


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--seeds", type=Path, required=True,
                    help="OPF-format training JSONL from compile_trainset.py.")
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--variants", type=int, default=10,
                    help="Format-preserving variants per positive seed.")
    ap.add_argument("--contexts", type=int, default=3,
                    help="Prose contexts per variant. Total = variants × contexts.")
    ap.add_argument("--negatives-per-seed", type=int, default=5,
                    help="Synthetic negatives generated per negative seed.")
    ap.add_argument("--limit", type=int, default=None,
                    help="Stop after processing N seeds (smoke test).")
    ap.add_argument("--model", default="gpt-5.4-mini",
                    help="Codex model. Default gpt-5.4-mini — small/cheap is fine "
                         "because the task is templated generation, not reasoning.")
    ap.add_argument("--parallel", type=int, default=8,
                    help="Concurrent codex exec calls. Codex-mini handles this fine; "
                         "each call is ~20s blocked on the API. Default 8.")
    ap.add_argument("--dry-run", action="store_true",
                    help="Print prompts but do not call codex.")
    args = ap.parse_args()

    with args.seeds.open("r") as f:
        seeds = [json.loads(line) for line in f if line.strip()]
    if args.limit:
        seeds = seeds[: args.limit]
    if not seeds:
        print("No seeds to expand.", file=sys.stderr)
        return 1

    args.out.parent.mkdir(parents=True, exist_ok=True)
    if args.dry_run:
        for i, seed in enumerate(seeds, start=1):
            print(f"--- seed {i} ---\n{_build_prompt(seed, args)}\n", file=sys.stderr)
        return 0

    total = len(seeds)
    n_written = 0
    n_rejected = 0
    lock = threading.Lock()

    print(f"Synthesizing from {total} seeds with {args.parallel}-way concurrency...",
          file=sys.stderr, flush=True)

    with args.out.open("w") as out_f, ThreadPoolExecutor(max_workers=args.parallel) as pool:
        futures = {
            pool.submit(_process_seed, i, total, seed, args): i
            for i, seed in enumerate(seeds, start=1)
        }
        completed = 0
        for fut in as_completed(futures):
            try:
                idx, valid, rejected = fut.result()
            except Exception as e:
                idx = futures[fut]
                print(f"  [seed {idx}] error: {e}", file=sys.stderr)
                continue
            with lock:
                for ex in valid:
                    out_f.write(json.dumps(ex, ensure_ascii=False) + "\n")
                out_f.flush()
                n_written += len(valid)
                n_rejected += len(rejected)
                completed += 1
                print(f"  [{completed}/{total}] seed {idx}: kept={len(valid)} "
                      f"rejected={len(rejected)}  running-total={n_written}",
                      file=sys.stderr, flush=True)

    print(f"\nDone. Wrote {n_written} synthetic examples "
          f"(rejected {n_rejected} malformed). Output: {args.out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
