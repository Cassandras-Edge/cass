"""Label space for OPF v2 (the taxonomy shipped by openai/privacy-filter).

Vendored subset of opf/_common/label_space.py — only the v2 category version
(what the released checkpoint uses). Future taxonomies (v4, v7) are not included.
"""
from __future__ import annotations

from typing import Final, Sequence

BACKGROUND_CLASS_LABEL: Final[str] = "O"
BOUNDARY_PREFIXES: Final[tuple[str, ...]] = ("B", "I", "E", "S")

SPAN_CLASS_NAMES_V2: Final[tuple[str, ...]] = (
    BACKGROUND_CLASS_LABEL,
    "account_number",
    "private_address",
    "private_date",
    "private_email",
    "private_person",
    "private_phone",
    "private_url",
    "secret",
)


def _expand_with_boundary_markers(span_class_names: Sequence[str]) -> tuple[str, ...]:
    """Expand span labels into token-level BIESO labels."""
    expanded: list[str] = [BACKGROUND_CLASS_LABEL]
    for base_label in span_class_names:
        if base_label == BACKGROUND_CLASS_LABEL:
            continue
        for prefix in BOUNDARY_PREFIXES:
            expanded.append(f"{prefix}-{base_label}")
    return tuple(expanded)


NER_CLASS_NAMES_V2: Final[tuple[str, ...]] = _expand_with_boundary_markers(SPAN_CLASS_NAMES_V2)
"""BIOES-expanded, 33 labels total (1 background + 8 span * 4 boundary)."""
