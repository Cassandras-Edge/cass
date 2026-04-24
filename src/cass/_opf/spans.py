"""Token-span → char-span helpers for OPF inference output.

Vendored from opf/_core/spans.py.
"""
from __future__ import annotations

from bisect import bisect_left, bisect_right
from typing import Protocol, Sequence

import tiktoken


def decode_text_with_offsets(
    token_ids: Sequence[int], encoding: tiktoken.Encoding
) -> tuple[str, list[int], list[int]]:
    """Decode tokens and return character offsets for the decoded text."""
    token_bytes = [
        encoding.decode_single_token_bytes(int(token_id)) for token_id in token_ids
    ]
    decoded_text = b"".join(token_bytes).decode("utf-8", errors="replace")
    if not token_bytes:
        return decoded_text, [], []

    char_byte_starts: list[int] = []
    char_byte_ends: list[int] = []
    byte_cursor = 0
    for ch in decoded_text:
        char_byte_starts.append(byte_cursor)
        byte_cursor += len(ch.encode("utf-8"))
        char_byte_ends.append(byte_cursor)

    char_starts: list[int] = []
    char_ends: list[int] = []
    token_byte_cursor = 0
    for raw_bytes in token_bytes:
        token_byte_start = token_byte_cursor
        token_byte_end = token_byte_start + len(raw_bytes)
        token_byte_cursor = token_byte_end
        start_idx = bisect_right(char_byte_ends, token_byte_start)
        end_idx = bisect_left(char_byte_starts, token_byte_end)
        if end_idx < start_idx:
            end_idx = start_idx
        char_starts.append(start_idx)
        char_ends.append(end_idx)
    return decoded_text, char_starts, char_ends


class LabelInfoLike(Protocol):
    """Subset of label-info fields needed for span reconstruction."""

    background_span_label: int
    token_to_span_label: dict[int, int]
    token_boundary_tags: dict[int, str | None]


def labels_to_spans(
    labels_by_index: dict[int, int], label_info: LabelInfoLike
) -> list[tuple[int, int, int]]:
    """Convert token-label ids into token-span tuples (label_idx, start, end)."""
    spans: list[tuple[int, int, int]] = []
    current_label: int | None = None
    start_idx: int | None = None
    previous_idx: int | None = None
    background_span_label = label_info.background_span_label

    for token_idx in sorted(labels_by_index):
        label_id = labels_by_index[token_idx]
        span_label = label_info.token_to_span_label.get(label_id)
        boundary_tag = label_info.token_boundary_tags.get(label_id)

        if previous_idx is not None and token_idx != previous_idx + 1:
            if current_label is not None and start_idx is not None:
                spans.append((current_label, start_idx, previous_idx + 1))
            current_label = None
            start_idx = None

        if span_label is None:
            previous_idx = token_idx
            continue

        is_background = span_label == background_span_label
        if is_background:
            if current_label is not None and start_idx is not None:
                spans.append((current_label, start_idx, token_idx))
            current_label = None
            start_idx = None
            previous_idx = token_idx
            continue

        if boundary_tag == "S":
            if current_label is not None and start_idx is not None and previous_idx is not None:
                spans.append((current_label, start_idx, previous_idx + 1))
            spans.append((span_label, token_idx, token_idx + 1))
            current_label = None
            start_idx = None
        elif boundary_tag == "B":
            if current_label is not None and start_idx is not None and previous_idx is not None:
                spans.append((current_label, start_idx, previous_idx + 1))
            current_label = span_label
            start_idx = token_idx
        elif boundary_tag == "I":
            if current_label is None or current_label != span_label:
                if current_label is not None and start_idx is not None and previous_idx is not None:
                    spans.append((current_label, start_idx, previous_idx + 1))
                current_label = span_label
                start_idx = token_idx
        elif boundary_tag == "E":
            if current_label is None or current_label != span_label or start_idx is None:
                if current_label is not None and start_idx is not None and previous_idx is not None:
                    spans.append((current_label, start_idx, previous_idx + 1))
                spans.append((span_label, token_idx, token_idx + 1))
                current_label = None
                start_idx = None
            else:
                spans.append((current_label, start_idx, token_idx + 1))
                current_label = None
                start_idx = None
        else:
            if current_label is not None and start_idx is not None and previous_idx is not None:
                spans.append((current_label, start_idx, previous_idx + 1))
            current_label = None
            start_idx = None
        previous_idx = token_idx

    if current_label is not None and start_idx is not None and previous_idx is not None:
        spans.append((current_label, start_idx, previous_idx + 1))
    return spans


def token_spans_to_char_spans(
    spans: Sequence[tuple[int, int, int]],
    char_starts: Sequence[int],
    char_ends: Sequence[int],
) -> list[tuple[int, int, int]]:
    """Convert token-index spans into character-index spans."""
    converted: list[tuple[int, int, int]] = []
    for label_idx, token_start, token_end in spans:
        if not (0 <= token_start < token_end <= len(char_starts)):
            continue
        char_start = char_starts[token_start]
        char_end = char_ends[token_end - 1]
        if char_end <= char_start:
            continue
        converted.append((label_idx, char_start, char_end))
    return converted


def trim_char_spans_whitespace(
    spans: Sequence[tuple[int, int, int]], text: str,
) -> list[tuple[int, int, int]]:
    """Trim leading and trailing whitespace from character spans."""
    trimmed: list[tuple[int, int, int]] = []
    for label_idx, start, end in spans:
        if not (0 <= start < end <= len(text)):
            continue
        while start < end and text[start].isspace():
            start += 1
        while end > start and text[end - 1].isspace():
            end -= 1
        if end > start:
            trimmed.append((label_idx, start, end))
    return trimmed
