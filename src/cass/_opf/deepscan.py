"""In-process OPF deep scan via mlx-embeddings on Apple Silicon.

Public surface:
    sanitize(text) -> str    # replaces detected PII spans with <LABEL> placeholders
    detect(text)  -> list[Span]  # returns spans without modifying text

The model + tokenizer are lazy-loaded on first call and cached for the process
lifetime (~2s cold load, ~10-25 ms per subsequent short input on M-series).
"""
from __future__ import annotations

import json
import platform
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path

from .label_space import NER_CLASS_NAMES_V2
from .sequence_labeling import build_label_info
from .spans import (
    decode_text_with_offsets,
    labels_to_spans,
    token_spans_to_char_spans,
    trim_char_spans_whitespace,
)
from .decoding import build_decoder, VITERBI_BIAS_KEYS


HF_REPO_ID = "openai/privacy-filter"


@dataclass(frozen=True)
class Span:
    label: str          # e.g. "secret", "private_email"
    start: int          # char offset in input text
    end: int
    text: str
    placeholder: str    # "<SECRET>", "<PRIVATE_EMAIL>"


class DeepScanUnavailable(RuntimeError):
    """Raised when mlx-embeddings isn't installed or not on Apple Silicon."""


def _require_apple_silicon() -> None:
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        raise DeepScanUnavailable(
            "OPF deep scan requires Apple Silicon (macOS arm64). "
            "Current platform: {}-{}. Use regex-only sanitization.".format(
                platform.system(), platform.machine()
            )
        )


def _label_placeholder(label: str) -> str:
    import re
    normalized = re.sub(r"[^A-Za-z0-9]+", "_", label.upper()).strip("_")
    return f"<{normalized or 'REDACTED'}>"


@lru_cache(maxsize=1)
def _load():
    """Load the MLX model + tokenizer + label info + decoder (cached).

    Returns: (model, tokenizer, label_info, decoder)
    """
    _require_apple_silicon()
    try:
        from mlx_embeddings.utils import load as _mlx_load  # type: ignore[import-not-found]
    except ImportError as e:
        raise DeepScanUnavailable(
            "mlx-embeddings not installed. Install with: "
            "pip install 'cass[deepscan]' — requires Apple Silicon."
        ) from e

    model, tokenizer = _mlx_load(HF_REPO_ID)
    label_info = build_label_info(NER_CLASS_NAMES_V2)

    # Try to read calibration from HF cache; fall back to all-zero biases.
    biases = _try_load_calibration()
    decoder = build_decoder(label_info, biases=biases)
    return model, tokenizer, label_info, decoder


def _try_load_calibration() -> dict[str, float] | None:
    """Find viterbi_calibration.json in HF cache; None if not found."""
    cache_root = Path.home() / ".cache" / "huggingface" / "hub" / f"models--{HF_REPO_ID.replace('/', '--')}"
    if not cache_root.exists():
        return None
    for candidate in cache_root.rglob("viterbi_calibration.json"):
        try:
            payload = json.loads(candidate.read_text())
            biases = payload["operating_points"]["default"]["biases"]
            return {key: float(biases[key]) for key in VITERBI_BIAS_KEYS}
        except (OSError, KeyError, ValueError, json.JSONDecodeError):
            continue
    return None


def detect(text: str) -> list[Span]:
    """Return detected PII spans. Empty list if text is empty."""
    if not text:
        return []

    model, tokenizer, label_info, decoder = _load()
    import mlx.core as mx
    import numpy as np

    encoded = tokenizer(text, return_tensors="np", padding=True, truncation=True)
    token_ids_np = encoded["input_ids"][0]
    input_ids = mx.array(encoded["input_ids"])
    attention_mask = mx.array(encoded["attention_mask"])

    out = model(input_ids, attention_mask=attention_mask)
    logits = out.logits if hasattr(out, "logits") else out[0]
    # MLX bf16 → fp32 before crossing to numpy; numpy has no bf16 and the
    # PEP-3118 buffer cast fails otherwise.
    logits_f32 = logits.astype(mx.float32)
    mx.eval(logits_f32)
    logits_np = np.asarray(logits_f32)[0]  # [seq_len, 33]
    # logsoftmax in numpy (keep it cheap, avoid torch):
    m = logits_np.max(axis=-1, keepdims=True)
    logprobs = logits_np - m - np.log(np.exp(logits_np - m).sum(axis=-1, keepdims=True))

    labels = decoder.decode(logprobs)
    labels_by_idx = {i: int(l) for i, l in enumerate(labels)}
    tok_spans = labels_to_spans(labels_by_idx, label_info)
    if not tok_spans:
        return []

    # Need tiktoken-style char offsets. mlx-embeddings' tokenizer may not be
    # tiktoken; reuse the OPF recipe via tiktoken on the token ids when we can,
    # otherwise fall back to the tokenizer's offset_mapping.
    char_starts, char_ends = _char_offsets(text, token_ids_np, tokenizer)

    char_spans = token_spans_to_char_spans(tok_spans, char_starts, char_ends)
    char_spans = trim_char_spans_whitespace(char_spans, text)

    out_spans: list[Span] = []
    for label_idx, s, e in char_spans:
        if 0 <= label_idx < len(label_info.span_class_names):
            name = label_info.span_class_names[label_idx]
        else:
            name = f"label_{label_idx}"
        if name == "O":
            continue
        out_spans.append(Span(
            label=name, start=s, end=e, text=text[s:e],
            placeholder=_label_placeholder(name),
        ))
    return out_spans


def _char_offsets(text: str, token_ids, tokenizer):
    """Return (char_starts, char_ends) for the given token ids.

    Tries tiktoken (matches OPF's native path) first — works when the
    tokenizer uses the o200k_base encoding. Falls back to re-tokenizing
    with return_offsets_mapping for HF tokenizers.
    """
    try:
        import tiktoken
        enc = tiktoken.get_encoding("o200k_base")
        decoded_text, starts, ends = decode_text_with_offsets(token_ids.tolist(), enc)
        if decoded_text == text:
            return starts, ends
    except Exception:
        pass

    # Fallback: HF offset mapping.
    encoded = tokenizer(
        text, return_tensors="np", padding=True, truncation=True,
        return_offsets_mapping=True,
    )
    offsets = encoded["offset_mapping"][0]
    starts = [int(s) for s, _ in offsets]
    ends = [int(e) for _, e in offsets]
    return starts, ends


def sanitize(text: str) -> str:
    """Replace every detected PII span with its typed placeholder.

    Raises DeepScanUnavailable if MLX / mlx-embeddings isn't usable.
    """
    spans = detect(text)
    if not spans:
        return text
    spans_sorted = sorted(spans, key=lambda s: s.start)
    out: list[str] = []
    cursor = 0
    for sp in spans_sorted:
        if sp.start < cursor:
            continue  # skip overlaps
        out.append(text[cursor:sp.start])
        out.append(sp.placeholder)
        cursor = sp.end
    out.append(text[cursor:])
    return "".join(out)
