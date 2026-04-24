"""LabelInfo mappings used by the Viterbi decoder and span postproc.

Vendored from opf/_core/sequence_labeling.py — only `LabelInfo` and
`build_label_info`. The windowing/aggregation helpers aren't needed because
we feed whole-sequence logits straight into the decoder.
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Mapping, Sequence

from .label_space import BACKGROUND_CLASS_LABEL, BOUNDARY_PREFIXES


@dataclass(frozen=True)
class LabelInfo:
    """Resolved label-space mappings used for inference and span decoding."""

    boundary_label_lookup: Mapping[str, Mapping[str, int]]
    token_to_span_label: Mapping[int, int]
    token_boundary_tags: Mapping[int, str | None]
    span_class_names: tuple[str, ...]
    span_label_lookup: Mapping[str, int]
    background_token_label: int
    background_span_label: int


def build_label_info(class_names: Sequence[str]) -> LabelInfo:
    """Build label-space lookup tables from the checkpoint class-name list."""
    span_class_names: list[str] = [BACKGROUND_CLASS_LABEL]
    span_label_lookup: dict[str, int] = {BACKGROUND_CLASS_LABEL: 0}
    boundary_label_lookup: dict[str, dict[str, int]] = {}
    token_to_span_label: dict[int, int] = {}
    token_boundary_tags: dict[int, str | None] = {}
    background_idx: int | None = None

    for idx, name in enumerate(class_names):
        if name == BACKGROUND_CLASS_LABEL:
            background_idx = idx
            token_to_span_label[idx] = span_label_lookup[BACKGROUND_CLASS_LABEL]
            token_boundary_tags[idx] = None
            continue
        boundary, base_label = name.split("-", 1)
        span_idx = span_label_lookup.get(base_label)
        if span_idx is None:
            span_idx = len(span_class_names)
            span_class_names.append(base_label)
            span_label_lookup[base_label] = span_idx
        token_to_span_label[idx] = span_idx
        token_boundary_tags[idx] = boundary
        mapping = boundary_label_lookup.setdefault(base_label, {})
        mapping[boundary] = idx

    if background_idx is None:
        raise ValueError("Class names must include background label 'O'")

    for base_label, mapping in boundary_label_lookup.items():
        missing = set(BOUNDARY_PREFIXES) - set(mapping)
        if missing:
            raise ValueError(
                f"Missing boundary classes {sorted(missing)} for base label {base_label}"
            )

    return LabelInfo(
        boundary_label_lookup={
            key: dict(value) for key, value in boundary_label_lookup.items()
        },
        token_to_span_label=dict(token_to_span_label),
        token_boundary_tags=dict(token_boundary_tags),
        span_class_names=tuple(span_class_names),
        span_label_lookup=dict(span_label_lookup),
        background_token_label=background_idx,
        background_span_label=span_label_lookup[BACKGROUND_CLASS_LABEL],
    )
