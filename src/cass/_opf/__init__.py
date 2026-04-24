"""Vendored subset of OpenAI Privacy Filter post-processing layer.

Source: https://github.com/openai/privacy-filter (Apache-2.0).

We depend on `mlx-embeddings` for the forward pass on Apple Silicon, and
vendor only the decode/span/label bits here. Keeps the `cass` install light
on non-Apple machines (the whole package is import-gated behind `deepscan`
extra).
"""
