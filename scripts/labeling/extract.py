#!/usr/bin/env python3
"""Walk Claude Code sessions and emit labeling candidates for OPF fine-tuning.

Usage:
    uv run python scripts/labeling/extract.py \
        --input ~/.claude/projects \
        --out /tmp/candidates.jsonl \
        [--detectors regex,opf] [--limit 200] [--min-turn-tokens 8]

Output: JSONL, one candidate-span per line:
    {
        "id": "<session-uuid>:<jsonl-line>:<span-start>",
        "session_file": "/abs/path/session.jsonl",
        "jsonl_line": 42,
        "role": "assistant",
        "kind": "text",
        "tag": "0",
        "context_before": "... ~64 chars leading up to span ...",
        "span_text": "wJalr...",
        "context_after": " ... ~64 chars after span ...",
        "regex_label": "<AWS_SECRET_KEY>" | null,
        "opf_label": "secret" | null,
        "opf_score": 0.98 | null,
        "source": "both" | "regex" | "opf"
    }

A downstream reviewer (CLI or web) consumes this file, asks a human
yes/no/label-override per candidate, and produces a training JSONL in OPF's
expected `{text, spans: [{start, end, label}]}` format.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator


# ── Regex detectors (mirror cass.share._sanitize but preserve span info) ─────

REGEX_RULES: list[tuple[str, re.Pattern]] = [
    ("openai_key",        re.compile(r"sk-[A-Za-z0-9_\-]{20,}")),
    ("github_token",      re.compile(r"ghp_[A-Za-z0-9]{30,}")),
    ("aws_access_key_id", re.compile(r"AKIA[0-9A-Z]{16}")),
    ("google_api_key",    re.compile(r"AIza[0-9A-Za-z_\-]{35}")),
    ("stripe_key",        re.compile(r"sk_live_[A-Za-z0-9]{20,}")),
    ("pem_private_key",   re.compile(
        r"-----BEGIN [A-Z ]+PRIVATE KEY-----[\s\S]+?-----END [A-Z ]+PRIVATE KEY-----")),
    ("jwt",               re.compile(
        r"eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+")),
    ("home_path",         re.compile(r"/Users/[a-zA-Z0-9._-]+")),
    ("private_ipv4",      re.compile(
        r"\b(?:10|127|172\.(?:1[6-9]|2\d|3[01])|192\.168)(?:\.\d{1,3}){3}\b")),
]


def _regex_spans(text: str) -> list[tuple[int, int, str]]:
    spans: list[tuple[int, int, str]] = []
    for name, pat in REGEX_RULES:
        for m in pat.finditer(text):
            spans.append((m.start(), m.end(), name))
    return spans


# ── JSONL session parsing ────────────────────────────────────────────────────

@dataclass
class Subunit:
    session_file: str
    jsonl_line: int
    role: str
    kind: str      # text / tool_use / tool_result
    tag: str
    text: str


def _extract_subunits(session_file: Path) -> Iterator[Subunit]:
    with session_file.open("r") as f:
        for line_idx, raw in enumerate(f, start=1):
            raw = raw.strip()
            if not raw:
                continue
            try:
                rec = json.loads(raw)
            except json.JSONDecodeError:
                continue
            m = rec.get("message")
            if not isinstance(m, dict):
                continue
            role = str(m.get("role", "unknown"))
            content = m.get("content")
            if isinstance(content, str):
                if content:
                    yield Subunit(str(session_file), line_idx, role, "text", "", content)
                continue
            if not isinstance(content, list):
                continue
            for i, part in enumerate(content):
                if not isinstance(part, dict):
                    continue
                t = part.get("type")
                if t == "text" or (t is None and "text" in part):
                    txt = part.get("text", "")
                    if isinstance(txt, str) and txt:
                        yield Subunit(str(session_file), line_idx, role, "text", str(i), txt)
                elif t == "tool_use":
                    inp = part.get("input")
                    if inp is not None:
                        try:
                            serial = json.dumps(inp, ensure_ascii=False)
                        except TypeError:
                            serial = str(inp)
                        if serial:
                            name = part.get("name", "tool")
                            yield Subunit(str(session_file), line_idx, role,
                                          "tool_use", str(name), serial)
                elif t == "tool_result":
                    tc = part.get("content")
                    if isinstance(tc, str) and tc:
                        yield Subunit(str(session_file), line_idx, role,
                                      "tool_result", str(i), tc)
                    elif isinstance(tc, list):
                        for j, y in enumerate(tc):
                            if isinstance(y, dict) and isinstance(y.get("text"), str) and y["text"]:
                                yield Subunit(str(session_file), line_idx, role,
                                              "tool_result", f"{i}.{j}", y["text"])


# ── Span merge ───────────────────────────────────────────────────────────────

def _merge_spans(
    text: str,
    regex_spans: list[tuple[int, int, str]],
    opf_spans: list,  # list[Span] from _opf.deepscan
) -> list[dict]:
    """Produce one record per distinct span location, carrying both labels if they overlap."""
    records: list[dict] = []

    # Index OPF spans by (start, end) for lookup.
    opf_by_range: dict[tuple[int, int], object] = {(s.start, s.end): s for s in opf_spans}

    seen_ranges: set[tuple[int, int]] = set()

    # Regex spans first.
    for s, e, rlabel in regex_spans:
        key = (s, e)
        seen_ranges.add(key)
        # Match an overlapping OPF span if it exists.
        matched_opf = None
        for (os_, oe), span in opf_by_range.items():
            if os_ < e and oe > s:  # any overlap
                matched_opf = span
                break
        records.append({
            "char_start": s, "char_end": e,
            "regex_label": rlabel,
            "opf_label": matched_opf.label if matched_opf else None,
            "opf_score": matched_opf.score if matched_opf else None,
            "source": "both" if matched_opf else "regex",
        })

    # OPF spans not matched by any regex.
    for (os_, oe), span in opf_by_range.items():
        overlaps_regex = any(rs < oe and re_ > os_ for rs, re_, _ in regex_spans)
        if overlaps_regex:
            continue
        records.append({
            "char_start": os_, "char_end": oe,
            "regex_label": None,
            "opf_label": span.label,
            "opf_score": span.score,
            "source": "opf",
        })

    records.sort(key=lambda r: r["char_start"])
    return records


# ── Context slicing ──────────────────────────────────────────────────────────

def _context(text: str, start: int, end: int, window: int) -> tuple[str, str]:
    cs = max(0, start - window)
    ce = min(len(text), end + window)
    before = text[cs:start].replace("\n", "\\n")
    after = text[end:ce].replace("\n", "\\n")
    if cs > 0:
        before = "…" + before
    if ce < len(text):
        after = after + "…"
    return before, after


# ── Main ─────────────────────────────────────────────────────────────────────

def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--input", type=Path, default=Path.home() / ".claude" / "projects",
                    help="Root dir to walk for *.jsonl session files.")
    ap.add_argument("--out", type=Path, required=True,
                    help="Output JSONL path for candidate spans.")
    ap.add_argument("--detectors", default="regex,opf",
                    help="Comma list: regex,opf. Default both.")
    ap.add_argument("--limit", type=int, default=None,
                    help="Stop after N session files (for smoke tests).")
    ap.add_argument("--min-turn-tokens", type=int, default=8,
                    help="Skip subunits with fewer than this many chars/4 approx tokens.")
    ap.add_argument("--context-chars", type=int, default=64,
                    help="How many chars of context to include around each span.")
    ap.add_argument("--max-subunit-chars", type=int, default=50_000,
                    help="Skip subunits longer than this (huge tool results blow OPF).")
    ap.add_argument("--progress-every", type=int, default=25,
                    help="Print progress every N session files.")
    args = ap.parse_args()

    detectors = {d.strip() for d in args.detectors.split(",") if d.strip()}
    use_opf = "opf" in detectors
    use_regex = "regex" in detectors

    if use_opf:
        try:
            from cass._opf.deepscan import detect as opf_detect
        except ImportError as e:
            print(f"OPF detector requested but unavailable: {e}", file=sys.stderr)
            print("Install with: pip install 'cass[deepscan]'  (macOS arm64 only)", file=sys.stderr)
            return 2

    args.out.parent.mkdir(parents=True, exist_ok=True)

    files = sorted(args.input.rglob("*.jsonl"))
    if args.limit:
        files = files[: args.limit]
    if not files:
        print(f"No .jsonl files found under {args.input}", file=sys.stderr)
        return 1

    print(f"Walking {len(files)} session files...", file=sys.stderr)
    total_candidates = 0
    subunits_scanned = 0
    subunits_skipped_large = 0

    with args.out.open("w") as out_f:
        for fi, session_file in enumerate(files, start=1):
            for unit in _extract_subunits(session_file):
                # Quick length filters.
                if len(unit.text) < args.min_turn_tokens * 4:
                    continue
                if len(unit.text) > args.max_subunit_chars:
                    subunits_skipped_large += 1
                    continue
                subunits_scanned += 1

                regex_spans = _regex_spans(unit.text) if use_regex else []
                opf_spans = []
                if use_opf:
                    try:
                        opf_spans = opf_detect(unit.text)
                    except Exception as e:
                        # Don't let one bad subunit kill the run.
                        print(f"  opf error on {session_file.name}:{unit.jsonl_line}: {e}",
                              file=sys.stderr)
                        opf_spans = []

                merged = _merge_spans(unit.text, regex_spans, opf_spans)
                for r in merged:
                    before, after = _context(
                        unit.text, r["char_start"], r["char_end"], args.context_chars
                    )
                    span_text = unit.text[r["char_start"]:r["char_end"]]
                    cand_id = hashlib.sha1(
                        f"{session_file.name}:{unit.jsonl_line}:{r['char_start']}:{span_text}".encode()
                    ).hexdigest()[:16]
                    rec = {
                        "id": cand_id,
                        "session_file": str(session_file),
                        "jsonl_line": unit.jsonl_line,
                        "role": unit.role,
                        "kind": unit.kind,
                        "tag": unit.tag,
                        "context_before": before,
                        "span_text": span_text,
                        "context_after": after,
                        "char_start": r["char_start"],
                        "char_end": r["char_end"],
                        "regex_label": r["regex_label"],
                        "opf_label": r["opf_label"],
                        "opf_score": r["opf_score"],
                        "source": r["source"],
                    }
                    out_f.write(json.dumps(rec, ensure_ascii=False) + "\n")
                    total_candidates += 1

            if fi % args.progress_every == 0:
                print(f"  [{fi}/{len(files)}] subunits={subunits_scanned} "
                      f"candidates={total_candidates} "
                      f"(skipped-large={subunits_skipped_large})", file=sys.stderr,
                      flush=True)

    print(f"\nDone. {total_candidates} candidate spans across "
          f"{subunits_scanned} subunits / {len(files)} session files "
          f"(skipped-large={subunits_skipped_large}).", file=sys.stderr)
    print(f"Output: {args.out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
