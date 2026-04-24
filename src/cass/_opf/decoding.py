"""Viterbi CRF decoder for OPF BIOES token labels.

Pure-numpy port of opf/_core/decoding.py. Enforces allowed BIOES boundary
transitions; scores complete paths with start/transition/end terms plus the
six transition biases from `viterbi_calibration.json`.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Mapping

import numpy as np

from .sequence_labeling import LabelInfo

_NEG_INF = -1e9

VITERBI_BIAS_KEYS: tuple[str, ...] = (
    "transition_bias_background_stay",
    "transition_bias_background_to_start",
    "transition_bias_inside_to_continue",
    "transition_bias_inside_to_end",
    "transition_bias_end_to_background",
    "transition_bias_end_to_start",
)


def zero_viterbi_transition_biases() -> dict[str, float]:
    return {key: 0.0 for key in VITERBI_BIAS_KEYS}


@dataclass
class ViterbiCRFDecoder:
    """Constrained Viterbi decoder over BIOES token classes."""

    label_info: LabelInfo
    transition_bias_background_stay: float = 0.0
    transition_bias_background_to_start: float = 0.0
    transition_bias_inside_to_continue: float = 0.0
    transition_bias_inside_to_end: float = 0.0
    transition_bias_end_to_background: float = 0.0
    transition_bias_end_to_start: float = 0.0

    _start_scores: np.ndarray = field(init=False, repr=False)
    _end_scores: np.ndarray = field(init=False, repr=False)
    _transition_scores: np.ndarray = field(init=False, repr=False)

    def __post_init__(self) -> None:
        num_classes = len(self.label_info.token_to_span_label)
        self._start_scores = np.full((num_classes,), _NEG_INF, dtype=np.float32)
        self._end_scores = np.full((num_classes,), _NEG_INF, dtype=np.float32)
        self._transition_scores = np.full(
            (num_classes, num_classes), _NEG_INF, dtype=np.float32
        )

        bg_tok = self.label_info.background_token_label
        bg_span = self.label_info.background_span_label
        boundary_tags = self.label_info.token_boundary_tags
        token_to_span = self.label_info.token_to_span_label

        for idx in range(num_classes):
            tag = boundary_tags.get(idx)
            span_label = token_to_span.get(idx)
            if tag in {"B", "S"} or idx == bg_tok:
                self._start_scores[idx] = 0.0
            if tag in {"E", "S"} or idx == bg_tok:
                self._end_scores[idx] = 0.0

            for next_idx in range(num_classes):
                next_tag = boundary_tags.get(next_idx)
                next_span = token_to_span.get(next_idx)
                if self._is_valid_transition(
                    prev_tag=tag, prev_span=span_label,
                    next_tag=next_tag, next_span=next_span,
                    background_token_idx=bg_tok, background_span_idx=bg_span,
                    next_idx=next_idx,
                ):
                    self._transition_scores[idx, next_idx] = self._transition_bias(
                        prev_tag=tag, prev_span=span_label,
                        next_tag=next_tag, next_span=next_span,
                        background_token_idx=bg_tok, background_span_idx=bg_span,
                        prev_idx=idx, next_idx=next_idx,
                    )

    def _transition_bias(
        self, *, prev_tag, prev_span, next_tag, next_span,
        background_token_idx, background_span_idx, prev_idx, next_idx,
    ) -> float:
        prev_is_bg = prev_span == background_span_idx or prev_idx == background_token_idx
        next_is_bg = next_span == background_span_idx or next_idx == background_token_idx

        if prev_is_bg:
            if next_is_bg:
                return self.transition_bias_background_stay
            if next_tag in {"B", "S"}:
                return self.transition_bias_background_to_start
            return 0.0
        if prev_tag in {"B", "I"}:
            if next_tag == "I" and prev_span == next_span:
                return self.transition_bias_inside_to_continue
            if next_tag == "E" and prev_span == next_span:
                return self.transition_bias_inside_to_end
            return 0.0
        if prev_tag in {"E", "S"}:
            if next_is_bg:
                return self.transition_bias_end_to_background
            if next_tag in {"B", "S"}:
                return self.transition_bias_end_to_start
            return 0.0
        return 0.0

    @staticmethod
    def _is_valid_transition(
        *, prev_tag, prev_span, next_tag, next_span,
        background_token_idx, background_span_idx, next_idx,
    ) -> bool:
        next_is_bg = next_span == background_span_idx or next_idx == background_token_idx
        if (next_span is None or next_tag is None) and not next_is_bg:
            return False
        if prev_span is None or prev_tag is None:
            return next_is_bg or next_tag in {"B", "S"}
        prev_is_bg = prev_span == background_span_idx
        if prev_is_bg:
            return next_is_bg or next_tag in {"B", "S"}
        if prev_tag in {"E", "S"}:
            return next_is_bg or next_tag in {"B", "S"}
        if prev_tag in {"B", "I"}:
            same = prev_span == next_span
            return same and next_tag in {"I", "E"}
        return False

    def decode(self, token_logprobs: np.ndarray) -> list[int]:
        """Decode a `[seq_len, num_classes]` logprob array → label ids."""
        if token_logprobs.ndim != 2:
            raise ValueError("token_logprobs must have shape [seq_len, num_classes]")
        seq_len, num_classes = token_logprobs.shape
        if seq_len == 0:
            return []

        scores = token_logprobs[0].astype(np.float32) + self._start_scores
        backpointers = np.empty((seq_len - 1, num_classes), dtype=np.int64)

        for idx in range(1, seq_len):
            # [num_classes, num_classes] = [prev, 1] + [prev, next]
            transitions = scores[:, None] + self._transition_scores
            best_paths = transitions.argmax(axis=0)
            best_scores = transitions.max(axis=0)
            scores = best_scores + token_logprobs[idx].astype(np.float32)
            backpointers[idx - 1] = best_paths

        if not np.isfinite(scores).any():
            # Pathological input — fall back to per-token argmax.
            return token_logprobs.argmax(axis=1).tolist()

        scores = scores + self._end_scores
        last_label = int(scores.argmax())
        path = np.empty((seq_len,), dtype=np.int64)
        path[-1] = last_label
        for idx in range(seq_len - 2, -1, -1):
            last_label = int(backpointers[idx, last_label])
            path[idx] = last_label
        return path.tolist()


def build_decoder(
    label_info: LabelInfo,
    biases: Mapping[str, float] | None = None,
) -> ViterbiCRFDecoder:
    """Construct a Viterbi decoder with the given (or zero) transition biases."""
    if biases is None:
        biases = zero_viterbi_transition_biases()
    return ViterbiCRFDecoder(label_info=label_info, **dict(biases))
