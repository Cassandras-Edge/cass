#!/usr/bin/env python3
"""Mine arbitrary local docs for OPF training candidates.

Walks configured source dirs, reads each text-like file, and emits candidate
PII/credential spans (positives + OPF false-positives for negative training).

Complements `extract.py` (which is Claude-Code-JSONL-specific). Same output
format so the downstream reviewer + training-set builder is shared.

Usage:
    uv run python scripts/labeling/mine_docs.py \
        --source ~/cassandra-stack/env \
        --source ~/cassandra-stack/cassandra-infra \
        --source ~/Documents/cassandra-notes \
        --out /tmp/mine.jsonl \
        [--detectors regex,opf]

Source-dir selection principle: point at places you expect real credentials /
PII to live. The gitignored `env/` directory is the canonical one. Obsidian
vaults, infra terraform, and operator runbooks are all useful. Do NOT point
at the whole home directory.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from pathlib import Path
from typing import Iterator


# ── Expanded regex ruleset: known credential formats + Cassandra-specific ──

REGEX_RULES: list[tuple[str, re.Pattern]] = [
    # Well-known cloud / platform credentials
    ("openai_key",         re.compile(r"sk-[A-Za-z0-9_\-]{20,}")),
    ("anthropic_key",      re.compile(r"sk-ant-[A-Za-z0-9_\-]{20,}")),
    ("github_pat",         re.compile(r"ghp_[A-Za-z0-9]{30,}")),
    ("github_fine_pat",    re.compile(r"github_pat_[A-Za-z0-9_]{60,}")),
    ("aws_access_key_id",  re.compile(r"AKIA[0-9A-Z]{16}")),
    ("aws_secret_access",  re.compile(r"(?<![A-Za-z0-9])[A-Za-z0-9/+=]{40}(?![A-Za-z0-9])")),
    ("google_api_key",     re.compile(r"AIza[0-9A-Za-z_\-]{35}")),
    ("stripe_secret",      re.compile(r"sk_live_[A-Za-z0-9]{20,}")),
    ("stripe_publish",     re.compile(r"pk_live_[A-Za-z0-9]{20,}")),
    ("slack_token",        re.compile(r"xox[abp]-[A-Za-z0-9-]{10,}")),
    ("pem_private_key",    re.compile(
        r"-----BEGIN [A-Z ]+PRIVATE KEY-----[\s\S]+?-----END [A-Z ]+PRIVATE KEY-----")),
    ("jwt",                re.compile(
        r"eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+")),
    ("cf_api_token",       re.compile(r"(?<![A-Za-z0-9])[A-Za-z0-9_\-]{40}(?![A-Za-z0-9_\-])")),
    ("cf_access_service",  re.compile(r"[0-9a-f]{32}\.access")),
    # Cassandra-specific formats (what our own stack issues)
    ("cass_mcp_key",       re.compile(r"mcp_[0-9a-f]{48}")),
    ("workos_secret",      re.compile(r"sk_test_[A-Za-z0-9_]{20,}|sk_WORKOS_[A-Za-z0-9_]{20,}")),
    # Personal / infra identifiers
    ("home_path",          re.compile(r"/Users/[a-zA-Z0-9._-]+")),
    ("private_ipv4",       re.compile(
        r"\b(?:10|127|172\.(?:1[6-9]|2\d|3[01])|192\.168)(?:\.\d{1,3}){3}\b")),
    ("cassandra_host",     re.compile(r"\b[a-z0-9\-]+\.cassandrasedge\.com\b")),
    ("tailnet_host",       re.compile(r"\b[a-z0-9\-]+\.cerberus-galaxy\.ts\.net\b")),
    ("email",              re.compile(r"\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b")),
    # Negative-candidate channels — patterns OPF over-flags as "secret" but
    # which typically aren't. We emit them with a `_negative` label so the
    # reviewer UI prioritizes them for "mark as background" in the training set.
    ("uuid_v4",            re.compile(
        r"\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b")),
    ("sha_hash",           re.compile(r"\b[0-9a-f]{40,64}\b")),
]

# Labels that should be treated as "flag for review as likely-not-secret".
NEGATIVE_CANDIDATE_LABELS = {"uuid_v4", "sha_hash", "home_path", "cassandra_host",
                             "tailnet_host", "cassandra_path"}


# ── Text-file ingestion ──────────────────────────────────────────────────────

TEXT_EXTENSIONS = {
    ".md", ".mdx", ".txt", ".rst", ".py", ".ts", ".tsx", ".js", ".jsx",
    ".go", ".rs", ".toml", ".yaml", ".yml", ".json", ".jsonl", ".tf",
    ".sh", ".bash", ".zsh", ".env", ".example", ".cfg", ".ini", ".conf",
    ".log", ".html", ".css", ".sql",
}
SKIP_DIR_NAMES = {
    ".git", ".venv", "venv", "node_modules", "__pycache__", "dist", "build",
    ".wrangler", ".cache", ".pytest_cache", ".mypy_cache", ".ruff_cache",
    "target", "vendor",
}


def _iter_text_files(root: Path, max_bytes: int) -> Iterator[Path]:
    for p in root.rglob("*"):
        if not p.is_file():
            continue
        if any(part in SKIP_DIR_NAMES for part in p.parts):
            continue
        # Size gate BEFORE extension check — cheaper.
        try:
            size = p.stat().st_size
        except OSError:
            continue
        if size == 0 or size > max_bytes:
            continue
        # .env + .example files have no suffix match; accept them explicitly.
        if p.suffix.lower() in TEXT_EXTENSIONS or p.name.endswith(".env") or p.name == ".env":
            yield p


def _safe_read(path: Path, max_bytes: int) -> str | None:
    try:
        data = path.read_bytes()[:max_bytes]
    except OSError:
        return None
    # Quick binary sniff
    if b"\x00" in data[:4096]:
        return None
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError:
        try:
            return data.decode("latin-1")
        except Exception:
            return None


# ── Shared candidate record builder ──────────────────────────────────────────

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


def _regex_spans(text: str) -> list[tuple[int, int, str]]:
    spans: list[tuple[int, int, str]] = []
    for name, pat in REGEX_RULES:
        for m in pat.finditer(text):
            spans.append((m.start(), m.end(), name))
    return spans


def _merge_spans(regex_spans, opf_spans):
    records: list[dict] = []
    opf_by_range = {(s.start, s.end): s for s in opf_spans}
    for s, e, rlabel in regex_spans:
        matched_opf = None
        for (os_, oe), span in opf_by_range.items():
            if os_ < e and oe > s:
                matched_opf = span
                break
        records.append({
            "char_start": s, "char_end": e,
            "regex_label": rlabel,
            "opf_label": matched_opf.label if matched_opf else None,
            "opf_score": matched_opf.score if matched_opf else None,
            "source": "both" if matched_opf else "regex",
            "negative_hint": rlabel in NEGATIVE_CANDIDATE_LABELS,
        })
    for (os_, oe), span in opf_by_range.items():
        if any(rs < oe and re_ > os_ for rs, re_, _ in regex_spans):
            continue
        records.append({
            "char_start": os_, "char_end": oe,
            "regex_label": None,
            "opf_label": span.label, "opf_score": span.score,
            "source": "opf", "negative_hint": False,
        })
    records.sort(key=lambda r: r["char_start"])
    return records


# ── Main ─────────────────────────────────────────────────────────────────────

def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--source", action="append", type=Path, required=True,
                    help="Dir to walk (can repeat).")
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--detectors", default="regex,opf",
                    help="Comma list: regex,opf. Default both.")
    ap.add_argument("--max-file-bytes", type=int, default=500_000,
                    help="Skip files larger than this (raw bytes).")
    ap.add_argument("--max-chars-per-run", type=int, default=20_000,
                    help="Slice long files into chunks this big before OPF.")
    ap.add_argument("--context-chars", type=int, default=64)
    ap.add_argument("--progress-every", type=int, default=100)
    args = ap.parse_args()

    detectors = {d.strip() for d in args.detectors.split(",") if d.strip()}
    use_opf = "opf" in detectors
    use_regex = "regex" in detectors
    opf_detect = None
    if use_opf:
        try:
            from cass._opf.deepscan import detect as opf_detect
        except ImportError as e:
            print(f"OPF unavailable: {e} — proceeding with regex only.", file=sys.stderr)
            use_opf = False

    args.out.parent.mkdir(parents=True, exist_ok=True)

    files: list[Path] = []
    for src in args.source:
        src = src.expanduser()
        if not src.exists():
            print(f"Source missing: {src}", file=sys.stderr)
            continue
        files.extend(_iter_text_files(src, args.max_file_bytes))
    files = sorted(set(files))

    if not files:
        print("No text files found in any --source.", file=sys.stderr)
        return 1

    print(f"Mining {len(files)} text files from "
          f"{', '.join(str(s) for s in args.source)}", file=sys.stderr)

    n_cand = 0
    n_files_with_hits = 0

    with args.out.open("w") as out_f:
        for fi, path in enumerate(files, start=1):
            content = _safe_read(path, args.max_file_bytes)
            if content is None:
                continue

            file_had_hits = False
            # Slice long files so OPF doesn't balloon memory.
            for chunk_start in range(0, len(content), args.max_chars_per_run):
                chunk = content[chunk_start:chunk_start + args.max_chars_per_run]

                regex_spans = _regex_spans(chunk) if use_regex else []
                opf_spans = []
                if use_opf and opf_detect is not None:
                    try:
                        opf_spans = opf_detect(chunk)
                    except Exception as e:
                        print(f"  opf error on {path.name}@{chunk_start}: {e}",
                              file=sys.stderr)

                for r in _merge_spans(regex_spans, opf_spans):
                    before, after = _context(
                        chunk, r["char_start"], r["char_end"], args.context_chars
                    )
                    span_text = chunk[r["char_start"]:r["char_end"]]
                    abs_start = chunk_start + r["char_start"]
                    abs_end = chunk_start + r["char_end"]
                    cand_id = hashlib.sha1(
                        f"{path}:{abs_start}:{span_text}".encode()
                    ).hexdigest()[:16]
                    rec = {
                        "id": cand_id,
                        "source_file": str(path),
                        "file_byte_start": abs_start,
                        "file_byte_end": abs_end,
                        "context_before": before,
                        "span_text": span_text,
                        "context_after": after,
                        "regex_label": r["regex_label"],
                        "opf_label": r["opf_label"],
                        "opf_score": r["opf_score"],
                        "source": r["source"],
                        "negative_hint": r["negative_hint"],
                    }
                    out_f.write(json.dumps(rec, ensure_ascii=False) + "\n")
                    n_cand += 1
                    file_had_hits = True

            if file_had_hits:
                n_files_with_hits += 1

            if fi % args.progress_every == 0:
                print(f"  [{fi}/{len(files)}] candidates={n_cand} "
                      f"files-with-hits={n_files_with_hits}", file=sys.stderr, flush=True)

    print(f"\nDone. {n_cand} candidates across {n_files_with_hits} files "
          f"(scanned {len(files)}).", file=sys.stderr)
    print(f"Output: {args.out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
