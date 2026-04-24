#!/usr/bin/env python3
"""Deterministically auto-label the unambiguous portion of a candidates file.

Purpose: bootstrap a seed training set without waiting for human review.
Applies high-confidence rules based on our mining analysis:
  • Known credential formats (ghp_, sk-, AKIA, pem, jwt, mcp_) → accept as "secret"
  • Known negative shapes (home_path, uuid_v4, sha_hash, private_ipv4) → reject
  • OPF-only categories with medium-plus confidence → accept at OPF's label
  • Ambiguous regex rules (cf_api_token, aws_secret_access) → skipped for human review

Output: same schema as review.py's decisions.jsonl, ready for
compile_trainset.py.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import sys
from pathlib import Path


# ── Rules ─────────────────────────────────────────────────────────────────────

# Regex labels that are effectively always real secrets of label "secret".
HIGH_CONFIDENCE_SECRET_REGEX = {
    "openai_key", "anthropic_key", "github_pat", "github_fine_pat",
    "google_api_key", "stripe_secret", "stripe_publish", "slack_token",
    "pem_private_key", "jwt", "cf_access_service", "cass_mcp_key",
    "workos_secret", "aws_access_key_id",
}

# Regex labels that map to a specific OPF category (not "secret").
REGEX_TO_OPF_LABEL = {
    "email": "private_email",
}

# Regex labels that indicate a NEGATIVE example (not PII/secret, just looks like it).
NEGATIVE_REGEX = {
    "home_path", "uuid_v4", "sha_hash", "private_ipv4",
    "cassandra_host", "tailnet_host",
}

# Regex labels too ambiguous for auto-label — punt to human review.
AMBIGUOUS_REGEX = {
    "cf_api_token",       # 40-char token pattern matches lots of non-tokens
    "aws_secret_access",  # 40-char base64 — even more ambiguous
}

# OPF-only accept threshold: above this score we trust the model's label.
OPF_ACCEPT_MIN_SCORE = 0.90


def _decide(cand: dict) -> tuple[str, str | None, str]:
    """Return (decision, final_label, rationale) for a candidate."""
    src = cand.get("source")
    rlbl = cand.get("regex_label")
    olbl = cand.get("opf_label")
    oscore = cand.get("opf_score") or 0.0

    # Negative regex patterns beat OPF disagreement — OPF systematically
    # mislabels home paths as private_person, uuids as secret, etc.
    if rlbl in NEGATIVE_REGEX:
        return "reject", "O", f"regex={rlbl} (known negative)"

    if rlbl in AMBIGUOUS_REGEX:
        return "skip", None, f"regex={rlbl} (ambiguous — human)"

    if rlbl in HIGH_CONFIDENCE_SECRET_REGEX:
        return "accept", "secret", f"regex={rlbl} (high-conf secret)"

    if rlbl in REGEX_TO_OPF_LABEL:
        return "accept", REGEX_TO_OPF_LABEL[rlbl], f"regex={rlbl}→{REGEX_TO_OPF_LABEL[rlbl]}"

    if src == "opf" and olbl and oscore >= OPF_ACCEPT_MIN_SCORE:
        return "accept", olbl, f"opf={olbl}@{oscore:.3f}"

    # Lower-confidence OPF-only: punt to human.
    return "skip", None, f"low-confidence (src={src}, opf_score={oscore:.3f})"


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--candidates", type=Path, required=True)
    ap.add_argument("--decisions", type=Path, required=True)
    ap.add_argument("--opf-min-score", type=float, default=OPF_ACCEPT_MIN_SCORE)
    args = ap.parse_args()

    with args.candidates.open("r") as f:
        cands = [json.loads(line) for line in f if line.strip()]

    decided = {"accept": 0, "reject": 0, "skip": 0}
    rationales: dict[str, int] = {}

    args.decisions.parent.mkdir(parents=True, exist_ok=True)
    with args.decisions.open("w") as out:
        for cand in cands:
            decision, label, rationale = _decide(cand)
            decided[decision] += 1
            rationales[rationale] = rationales.get(rationale, 0) + 1
            out.write(json.dumps({
                "id": cand["id"],
                "decision": decision,
                "final_label": label,
                "span_text": cand.get("span_text", ""),
                "context_before": cand.get("context_before", ""),
                "context_after": cand.get("context_after", ""),
                "source_file": cand.get("source_file") or cand.get("session_file"),
                "reviewed_at": dt.datetime.now(dt.timezone.utc).isoformat(),
                "auto": True, "rationale": rationale,
            }) + "\n")

    print(f"Decisions: accept={decided['accept']}  reject={decided['reject']}  "
          f"skip={decided['skip']}  total={sum(decided.values())}", file=sys.stderr)
    print(file=sys.stderr)
    print("Rationale breakdown:", file=sys.stderr)
    for k, v in sorted(rationales.items(), key=lambda x: -x[1]):
        print(f"  {v:5} {k}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
