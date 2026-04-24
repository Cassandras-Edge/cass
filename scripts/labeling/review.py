#!/usr/bin/env python3
"""CLI reviewer for OPF training candidates.

Reads a candidates JSONL from extract.py or mine_docs.py, presents each span
to a human with surrounding context, and records labeling decisions to a
decisions JSONL. Resumable — re-run with the same --decisions path and it
skips already-reviewed candidate IDs.

Usage:
    uv run python scripts/labeling/review.py \
        --candidates /tmp/mine.jsonl \
        --decisions /tmp/decisions.jsonl \
        [--sort priority|confidence|file] \
        [--filter-source opf|regex|both]

Keyboard:
    y/Enter   accept the proposed label as correct
    n/space   reject — this is NOT a secret/PII (negative training example)
    l         relabel — pick a different label from the OPF taxonomy
    s         skip — no decision, move on (can revisit later)
    p         go back to previous candidate
    ?         show keys
    q         save + quit

Decisions JSONL schema:
    {
      "id": "<candidate-id>",
      "decision": "accept" | "reject" | "relabel" | "skip",
      "final_label": "secret" | "private_email" | ... | null,
      "span_text": "...",
      "context_before": "...", "context_after": "...",
      "source_file": "...",
      "reviewed_at": "2026-04-24T..."
    }
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import sys
from pathlib import Path

import click


OPF_LABELS = (
    "account_number", "private_address", "private_date", "private_email",
    "private_person", "private_phone", "private_url", "secret",
)
BACKGROUND_LABEL = "O"  # reject / not a secret


# ── TUI helpers ──────────────────────────────────────────────────────────────

def _color(code: str, s: str) -> str:
    return f"\033[{code}m{s}\033[0m" if sys.stdout.isatty() else s

RED = lambda s: _color("31;1", s)
GRN = lambda s: _color("32;1", s)
YLW = lambda s: _color("33", s)
DIM = lambda s: _color("2", s)
BLD = lambda s: _color("1", s)
CYN = lambda s: _color("36", s)


def _render_candidate(cand: dict, index: int, total: int, stats: dict) -> None:
    # Header
    pct = 100.0 * (stats["accept"] + stats["reject"] + stats["relabel"]) / max(1, total)
    print("─" * 78)
    print(f"{BLD(f'[{index+1}/{total}]')}  "
          f"{GRN(f'accept={stats[\"accept\"]}')} "
          f"{RED(f'reject={stats[\"reject\"]}')} "
          f"{YLW(f'relabel={stats[\"relabel\"]}')} "
          f"{DIM(f'skip={stats[\"skip\"]}  ({pct:.0f}% decided)')}")
    print()

    # Span meta
    src = cand.get("source", "?")
    regex_lbl = cand.get("regex_label") or "—"
    opf_lbl = cand.get("opf_label") or "—"
    opf_score = cand.get("opf_score")
    score_str = f"{opf_score:.3f}" if isinstance(opf_score, (int, float)) else "—"
    neg_hint = cand.get("negative_hint", False)
    neg_str = f" {YLW('[likely-negative]')}" if neg_hint else ""

    print(f"  source: {CYN(src)}{neg_str}")
    print(f"  regex : {regex_lbl}")
    print(f"  opf   : {opf_lbl}  (score {score_str})")
    origin_file = cand.get("session_file") or cand.get("source_file") or "?"
    print(f"  from  : {DIM(origin_file)}")
    print()

    # Context window
    before = cand.get("context_before", "")
    after = cand.get("context_after", "")
    span = cand.get("span_text", "")
    print(f"  {DIM(before)}{RED(span)}{DIM(after)}")
    print()


def _render_help() -> None:
    print()
    print(BLD("Keys:"))
    print(f"  {GRN('y')} / Enter   accept proposed label")
    print(f"  {RED('n')} / space   reject — not a secret (negative example)")
    print(f"  {YLW('l')}           relabel — pick a different label")
    print(f"  {CYN('s')}           skip — decide later")
    print(f"  {CYN('p')}           previous candidate")
    print(f"  {CYN('?')}           this help")
    print(f"  {CYN('q')}           save + quit")
    print()


def _choose_label() -> str | None:
    print()
    for i, lbl in enumerate(OPF_LABELS, start=1):
        print(f"  {i}. {lbl}")
    print(f"  0. {BACKGROUND_LABEL} (not a secret — reject)")
    print()
    raw = click.prompt("Label number (or enter to cancel)", default="", show_default=False)
    if not raw.strip():
        return None
    try:
        n = int(raw.strip())
    except ValueError:
        return None
    if n == 0:
        return BACKGROUND_LABEL
    if 1 <= n <= len(OPF_LABELS):
        return OPF_LABELS[n - 1]
    return None


def _proposed_label(cand: dict) -> str:
    """What 'accept' means for this candidate — pick the best available label."""
    if cand.get("opf_label"):
        return str(cand["opf_label"])
    rlbl = cand.get("regex_label")
    if rlbl:
        # Map regex rule names to OPF labels where obvious.
        mapping = {
            "openai_key": "secret", "anthropic_key": "secret", "github_pat": "secret",
            "github_fine_pat": "secret", "aws_access_key_id": "secret",
            "aws_secret_access": "secret", "google_api_key": "secret",
            "stripe_secret": "secret", "stripe_publish": "secret", "slack_token": "secret",
            "pem_private_key": "secret", "jwt": "secret", "cf_api_token": "secret",
            "cf_access_service": "secret", "cass_mcp_key": "secret",
            "workos_secret": "secret",
            "home_path": BACKGROUND_LABEL,  # paths default to negative
            "private_ipv4": BACKGROUND_LABEL,
            "cassandra_host": "private_url",
            "tailnet_host": "private_url",
            "email": "private_email",
            "uuid_v4": BACKGROUND_LABEL, "sha_hash": BACKGROUND_LABEL,
        }
        return mapping.get(rlbl, "secret")
    return "secret"


# ── Sort + filter ────────────────────────────────────────────────────────────

def _sort_key(sort_mode: str):
    if sort_mode == "confidence":
        # High-confidence OPF hits first — fastest to triage
        def key(c):
            score = c.get("opf_score") or 0.0
            return (-score, c.get("source_file", ""), c.get("char_start", 0))
        return key
    if sort_mode == "file":
        def key(c):
            return (c.get("source_file", "") or c.get("session_file", ""),
                    c.get("char_start", 0))
        return key
    # default: priority — both > opf > regex, then negative-hints last
    def key(c):
        source_order = {"both": 0, "opf": 1, "regex": 2}
        neg = 1 if c.get("negative_hint") else 0
        return (neg, source_order.get(c.get("source"), 3),
                -(c.get("opf_score") or 0.0))
    return key


# ── Main ─────────────────────────────────────────────────────────────────────

def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--candidates", type=Path, required=True)
    ap.add_argument("--decisions", type=Path, required=True,
                    help="Append-only JSONL log of decisions (resumable).")
    ap.add_argument("--sort", choices=["priority", "confidence", "file"], default="priority")
    ap.add_argument("--filter-source", choices=["opf", "regex", "both", "all"], default="all")
    ap.add_argument("--filter-label",
                    help="Only show candidates where opf_label or regex_label contains this substring.")
    args = ap.parse_args()

    if not args.candidates.exists():
        print(f"Missing candidates file: {args.candidates}", file=sys.stderr)
        return 1

    # Load candidates
    with args.candidates.open("r") as f:
        candidates = [json.loads(line) for line in f if line.strip()]
    if args.filter_source != "all":
        candidates = [c for c in candidates if c.get("source") == args.filter_source]
    if args.filter_label:
        needle = args.filter_label.lower()
        candidates = [c for c in candidates if needle in
                      ((c.get("opf_label") or "") + (c.get("regex_label") or "")).lower()]
    candidates.sort(key=_sort_key(args.sort))

    # Load existing decisions for resume
    args.decisions.parent.mkdir(parents=True, exist_ok=True)
    reviewed: dict[str, str] = {}  # id -> decision
    if args.decisions.exists():
        with args.decisions.open("r") as f:
            for line in f:
                if not line.strip():
                    continue
                try:
                    rec = json.loads(line)
                    reviewed[rec["id"]] = rec["decision"]
                except Exception:
                    continue

    remaining = [c for c in candidates if c["id"] not in reviewed]
    total = len(remaining)
    if total == 0:
        print(GRN("All candidates already reviewed. Nothing to do."))
        return 0

    stats = {
        "accept": sum(1 for d in reviewed.values() if d == "accept"),
        "reject": sum(1 for d in reviewed.values() if d == "reject"),
        "relabel": sum(1 for d in reviewed.values() if d == "relabel"),
        "skip": sum(1 for d in reviewed.values() if d == "skip"),
    }

    print(BLD(f"Review queue: {total} candidates "
              f"({len(reviewed)} already reviewed)"))
    _render_help()

    # Open decisions file in append mode for streaming writes.
    decisions_f = args.decisions.open("a")

    def write_decision(cand: dict, decision: str, final_label: str | None) -> None:
        rec = {
            "id": cand["id"],
            "decision": decision,
            "final_label": final_label,
            "span_text": cand.get("span_text", ""),
            "context_before": cand.get("context_before", ""),
            "context_after": cand.get("context_after", ""),
            "source_file": cand.get("source_file") or cand.get("session_file"),
            "reviewed_at": dt.datetime.now(dt.timezone.utc).isoformat(),
        }
        decisions_f.write(json.dumps(rec, ensure_ascii=False) + "\n")
        decisions_f.flush()
        stats[decision] += 1

    try:
        i = 0
        while i < len(remaining):
            cand = remaining[i]
            _render_candidate(cand, i, total, stats)
            proposed = _proposed_label(cand)
            print(f"  Accept → label: {CYN(proposed)}", end="   ")

            key = click.getchar(echo=False).lower()
            print()

            if key in ("y", "\r", "\n"):
                write_decision(cand, "accept", proposed)
                i += 1
            elif key in ("n", " "):
                write_decision(cand, "reject", BACKGROUND_LABEL)
                i += 1
            elif key == "l":
                chosen = _choose_label()
                if chosen is None:
                    continue  # stay on same candidate
                write_decision(cand, "relabel", chosen)
                i += 1
            elif key == "s":
                write_decision(cand, "skip", None)
                i += 1
            elif key == "p":
                i = max(0, i - 1)
            elif key == "?":
                _render_help()
            elif key == "q":
                break
            else:
                print(f"  {DIM('(unknown key — ? for help)')}")
    finally:
        decisions_f.close()

    print()
    print(BLD("Session summary:"))
    print(f"  accepted:  {stats['accept']}")
    print(f"  rejected:  {stats['reject']}")
    print(f"  relabeled: {stats['relabel']}")
    print(f"  skipped:   {stats['skip']}")
    print(f"  Decisions log: {args.decisions}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
