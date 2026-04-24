# OPF training pipeline

Four-stage workflow for producing a Cassandra-specific OPF checkpoint that
catches the credential formats we actually use and ignores the things we
spuriously get flagged for.

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│ extract.py   │────▶│ review.py    │────▶│ compile_     │────▶│ synthesize.py│
│ mine_docs.py │     │ (CLI)        │     │ trainset.py  │     │ (codex mini) │
└──────────────┘     └──────────────┘     └──────────────┘     └──────────────┘
    candidates         decisions            train.jsonl          + synthetic
     .jsonl             .jsonl              (seeds only)         variants
                                                                       │
                                                                       ▼
                                                              ┌─────────────┐
                                                              │ concat +    │
                                                              │ opf train   │
                                                              └─────────────┘
```

## 1. Mine candidate spans

Two sources, same output schema.

```bash
# Claude Code sessions — the biggest source
uv run python scripts/labeling/extract.py \
    --input ~/.claude/projects \
    --out /tmp/candidates-sessions.jsonl \
    --detectors regex,opf

# Arbitrary docs (env/, terraform, obsidian, runbooks, etc.)
uv run python scripts/labeling/mine_docs.py \
    --source ~/cassandra-stack/env \
    --source ~/cassandra-stack/cassandra-infra \
    --source ~/Documents/cassandra-notes \
    --out /tmp/candidates-docs.jsonl \
    --detectors regex,opf

# Concatenate into one queue
cat /tmp/candidates-sessions.jsonl /tmp/candidates-docs.jsonl > /tmp/candidates.jsonl
```

`--detectors regex,opf` runs both the regex pass and in-process MLX OPF.
Regex-only is faster but misses context-aware catches; OPF alone misses
well-known token formats like `AKIA…`. Run both, merge, dedup happens
automatically (overlapping hits become `source: "both"`).

## 2. Review candidates

Interactive CLI, resumable.

```bash
uv run python scripts/labeling/review.py \
    --candidates /tmp/candidates.jsonl \
    --decisions /tmp/decisions.jsonl \
    --sort priority
```

Keyboard: `y`=accept, `n`=reject (negative), `l`=relabel, `s`=skip, `p`=back, `q`=quit.
Resume by rerunning — already-reviewed IDs are skipped automatically.

Recommended filters:
- `--filter-source both` — high-confidence first (regex AND OPF both fired)
- `--filter-label secret` — triage all the secret-flagged ones together
- `--sort confidence` — OPF-confidence descending, fastest to triage

Target: 200–500 accepted positives + 200–500 rejected negatives. That's the
seed set.

## 3. Compile seed training set

Turn decisions into OPF's expected `{text, spans: [...]}` JSONL.

```bash
uv run python scripts/labeling/compile_trainset.py \
    --decisions /tmp/decisions.jsonl \
    --out /tmp/train-seeds.jsonl
```

- `accept` and `relabel` → positive example with the labeled span
- `reject` → negative example (empty `spans` list)
- `skip` → filtered out (unless `--include-skipped`)

## 4. Expand with synthetic data

For each seed, codex generates format-preserving variants + prose contexts.
Default model is `gpt-5.4-mini` — cheap and plenty for templated generation.

```bash
uv run python scripts/labeling/synthesize.py \
    --seeds /tmp/train-seeds.jsonl \
    --out /tmp/train-synthetic.jsonl \
    --variants 10 --contexts 3
```

Produces `variants × contexts` examples per positive seed (30 by default), and
`--negatives-per-seed` examples per negative seed (5 by default). For a 500-seed
input that's ~15k synthetic examples.

Final training set: `cat /tmp/train-seeds.jsonl /tmp/train-synthetic.jsonl > /tmp/train.jsonl`

## 5. Train

Upstream OPF `train` command — easiest on Modal H100.

```bash
# (requires the opf package from openai/privacy-filter)
opf train /tmp/train.jsonl \
    --output-dir ~/.opf/privacy-filter-cass/
```

Point `cass._opf.deepscan.HF_REPO_ID` at the new checkpoint (or push to a
private HF repo and update the constant).

## Data volume targets

Per the OPF model card's fine-tuning efficiency table:

| Examples | F1 (tokens) |
|---|---|
| 300 | 0.879 |
| 3,000 | 0.962 |
| 15,000 | 0.983 |

**Sweet spot: 2–4k total examples** (seeds + synthetic). Hits near-saturated F1
without burning too much compute. We can get to 2k in an afternoon of
reviewing + synthesis.
