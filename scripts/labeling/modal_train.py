"""Fine-tune OPF on Modal H100.

Uploads the prepared train/val/test JSONL from finalize_dataset.py, runs
`opf train` inside a CUDA image, and publishes the resulting checkpoint back
to a Modal Volume for download.

Usage:
    modal run scripts/labeling/modal_train.py \\
        --dataset-dir /tmp/opf-trainset/ \\
        --output-name privacy-filter-cass-v1

Design notes:
- Reuses the `opf-weights` Modal Volume we already stood up for `modal_bench`.
- Mounts the local dataset dir as input; writes checkpoint artifacts to a
  separate `opf-checkpoints` Volume.
- Uses the same image as modal_bench (torch + triton + tiktoken) plus a pip
  install of `opf` from git (we need its `train` entrypoint).
"""
from __future__ import annotations

import modal

app = modal.App("opf-train")

image = (
    modal.Image.debian_slim(python_version="3.12")
    .apt_install("git")
    .pip_install(
        "torch==2.6.0",
        "triton==3.2.0",
        "safetensors",
        "tiktoken",
        "huggingface_hub",
        "numpy",
        "packaging",
        "datasets",
    )
    .run_commands(
        "pip install 'opf @ git+https://github.com/openai/privacy-filter.git@main' --no-deps"
    )
)

weights_vol = modal.Volume.from_name("opf-weights", create_if_missing=True)
checkpoints_vol = modal.Volume.from_name("opf-checkpoints", create_if_missing=True)


def _download_base_checkpoint() -> str:
    """Ensure the base openai/privacy-filter checkpoint is in /weights/pf_v2."""
    import shutil
    from pathlib import Path
    from huggingface_hub import snapshot_download

    target = Path("/weights/pf_v2")
    marker = target / "config.json"
    if marker.exists():
        import json
        cfg = json.loads(marker.read_text())
        if cfg.get("model_type") == "privacy_filter":
            return str(target)

    target.mkdir(parents=True, exist_ok=True)
    snapshot_download(
        repo_id="openai/privacy-filter",
        local_dir=str(target),
        allow_patterns=["original/*"],
    )
    orig = target / "original"
    if orig.is_dir():
        for p in orig.iterdir():
            dest = target / p.name
            if dest.exists():
                continue
            shutil.move(str(p), str(dest))
        orig.rmdir()
    return str(target)


@app.function(
    image=image,
    volumes={"/weights": weights_vol, "/checkpoints": checkpoints_vol},
    gpu="H100",
    timeout=60 * 60 * 4,  # 4 hours — overshoots any reasonable fine-tune on 2-10k examples
)
def train(
    train_bytes: bytes,
    val_bytes: bytes,
    output_name: str,
    epochs: int = 3,
    batch_size: int = 8,
    learning_rate: float = 1e-5,
) -> dict:
    import json, os, subprocess, time
    from pathlib import Path

    os.environ["OPF_MOE_TRITON"] = "1"

    base_ckpt = _download_base_checkpoint()
    weights_vol.commit()

    data_dir = Path("/data")
    data_dir.mkdir(parents=True, exist_ok=True)
    (data_dir / "train.jsonl").write_bytes(train_bytes)
    (data_dir / "val.jsonl").write_bytes(val_bytes)

    output_dir = Path(f"/checkpoints/{output_name}")
    output_dir.mkdir(parents=True, exist_ok=True)

    print(f"Base checkpoint: {base_ckpt}")
    print(f"Train: {len(train_bytes)} bytes")
    print(f"Val:   {len(val_bytes)} bytes")
    print(f"Output: {output_dir}")

    # Invoke opf train — the exact flags depend on the shipped CLI. We pass
    # checkpoint, data, output, epochs, batch, lr. If the CLI rejects a flag
    # name, adjust once per run (output captured below for iteration).
    cmd = [
        "python", "-m", "opf", "train",
        str(data_dir / "train.jsonl"),
        "--checkpoint", base_ckpt,
        "--output-dir", str(output_dir),
        "--epochs", str(epochs),
        "--batch-size", str(batch_size),
        "--learning-rate", str(learning_rate),
    ]
    print("Running:", " ".join(cmd))
    t0 = time.time()
    r = subprocess.run(cmd, capture_output=True, text=True)
    elapsed = time.time() - t0

    checkpoints_vol.commit()

    summary = {
        "output_name": output_name,
        "output_dir": str(output_dir),
        "elapsed_s": round(elapsed, 1),
        "returncode": r.returncode,
        "stdout_tail": r.stdout[-4000:],
        "stderr_tail": r.stderr[-4000:],
        "artifacts": sorted(p.name for p in output_dir.iterdir()) if output_dir.exists() else [],
    }
    print(json.dumps(summary, indent=2)[:3000])
    return summary


@app.local_entrypoint()
def main(dataset_dir: str, output_name: str = "privacy-filter-cass-v1",
         epochs: int = 3, batch_size: int = 8, learning_rate: float = 1e-5):
    """Upload dataset + trigger remote training."""
    from pathlib import Path
    import json

    dd = Path(dataset_dir).expanduser()
    train_p = dd / "train.jsonl"
    val_p = dd / "val.jsonl"
    if not train_p.exists() or not val_p.exists():
        raise SystemExit(f"Missing train.jsonl or val.jsonl in {dd}")

    print(f"Dataset: {dd}")
    print(f"  train: {train_p.stat().st_size:,} bytes")
    print(f"  val:   {val_p.stat().st_size:,} bytes")

    result = train.remote(
        train_bytes=train_p.read_bytes(),
        val_bytes=val_p.read_bytes(),
        output_name=output_name,
        epochs=epochs, batch_size=batch_size, learning_rate=learning_rate,
    )
    print(json.dumps(result, indent=2)[:5000])
