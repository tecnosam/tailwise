"""SWE-agent trajectories (nebius/SWE-agent-trajectories, CC-BY-4.0):
coding agents fixing real GitHub issues, 80k runs.

`target` is true when the agent's patch made the issue's tests pass; a run
with target false failed. A seeded sample is drawn from `shards` parquet
files starting at `first_shard`, at the natural failure rate.

A trajectory alternates `ai` messages (reasoning ending in one fenced
command) and `user` messages (the command's output). Tailwise sees
SWE-agent's system message as the system prompt, the issue as the question,
every command with its output as a tool call, and the submitted patch as the
final answer.
"""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any

import numpy as np
import pyarrow.parquet as pq
from huggingface_hub import hf_hub_download

from ..otel import hex_id
from ..types import Run, ToolCall

COLUMNS = ["instance_id", "model_name", "target", "trajectory", "exit_status", "generated_patch"]
_FENCE = re.compile(r"```[^\n]*\n(.*?)```", re.DOTALL)


def download(repo: str, shard: int, shards_total: int, cache: Path) -> Path:
    name = f"data/train-{shard:05d}-of-{shards_total:05d}.parquet"
    return Path(hf_hub_download(repo, name, repo_type="dataset", local_dir=cache / "hf" / repo))


def load(repo: str, shards: int, shards_total: int, sample: int, seed: int, cache: Path,
         limit: int | None = None, first_shard: int = 0) -> tuple[list[Run], dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for shard in range(first_shard, first_shard + shards):
        rows += pq.read_table(download(repo, shard, shards_total, cache), columns=COLUMNS).to_pylist()
    n = min(sample, len(rows)) if limit is None else min(limit, sample, len(rows))
    picked = sorted(np.random.default_rng(seed).choice(len(rows), size=n, replace=False).tolist())
    runs = [parse(rows[i], i) for i in picked]
    return runs, {"pool": len(rows), "runs": len(runs), "failed": sum(r.failed for r in runs), "shards": shards,
                  "first_shard": first_shard}


def issue_text(first_user: str) -> str:
    """The issue itself, without SWE-agent's instructions around it."""
    m = re.search(r"ISSUE:\n(.*?)\n+INSTRUCTIONS:", first_user, re.DOTALL)
    return m.group(1).strip() if m else first_user.strip()


def command_of(ai_text: str) -> str:
    blocks = _FENCE.findall(ai_text)
    return blocks[-1].strip() if blocks else ""


def parse(row: dict[str, Any], index: int) -> Run:
    for key in COLUMNS:
        if key not in row:
            raise ValueError(f"SWE-agent row {index}: no {key!r} (keys {sorted(row)})")
    traj = row["trajectory"] or []
    question, calls = "", []
    system = next((m.get("system_prompt") or m.get("text") or "" for m in traj if m.get("role") == "system"), "")
    pending: str | None = None
    for m in traj:
        role, text = m.get("role"), m.get("text") or ""
        if role == "user" and not question:
            question = issue_text(text)
        elif role == "ai":
            if pending is not None:
                calls.append(_call(pending, ""))
            pending = command_of(text)
        elif role == "user" and pending is not None:
            calls.append(_call(pending, text))
            pending = None
    if pending:
        calls.append(_call(pending, ""))
    run_id = f"{row['instance_id']}:{row['model_name']}:{index}"
    return Run(
        dataset="swe", subset=str(row["model_name"]), run_id=run_id, trace_id=hex_id("swe", run_id),
        failed=not bool(row["target"]), system_prompt=system, question=question, tool_calls=calls,
        final_answer=str(row["generated_patch"] or ""),
        meta={"instance_id": row["instance_id"], "model": row["model_name"], "exit_status": row["exit_status"]},
    )


def _call(command: str, output: str) -> ToolCall:
    name = command.split(maxsplit=1)[0] if command.strip() else "(no command)"
    return ToolCall(name=name, args={"command": command}, output=output)
