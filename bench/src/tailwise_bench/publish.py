"""The published figures: bench/published/benchmark.json, read by the website
at the latest release's tag. One row per dataset slice and setup, at the
cost section's deployment (report.COST_TRACES, COST_FAILURE_RATE)."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .report import COST_FAILURE_RATE, COST_TRACES, FEW_REVIEWED, _review_at

SCHEMA = 1

DATASETS = {
    "tau": {"name": "τ-bench", "agent": "Customer-service agents (GPT-4o, Claude 3.5 Sonnet) in airline and retail "
                                         "simulations, acting through tools for a simulated user"},
    "swe": {"name": "SWE-agent", "agent": "SWE-agent on Llama models (8B, 70B, 405B) fixing GitHub issues; a run fails "
                                          "when its patch does not pass the tests"},
    "trail": {"name": "TRAIL", "agent": "smolagents runs on GAIA and SWE-bench, with failures annotated by hand"},
}


def _row(ds: str, part: str, s: dict[str, Any], meta: dict[str, Any]) -> dict[str, Any] | None:
    budget = meta["config"].get("review_budget", 0.05)
    at = _review_at(s["default"], COST_FAILURE_RATE)
    if not at or s.get("cost_per_run_usd") is None:
        return None
    reviews = round(COST_TRACES * budget)
    random_found = reviews * COST_FAILURE_RATE
    found = reviews * at["precision"]
    jev = s["cost_per_run_usd"] * COST_TRACES
    variant = meta["variant"]
    gain = at.get("vs_baseline")
    return {
        "dataset": ds,
        "slice": part,
        "setup": variant.removesuffix("+rules"),
        "rules": variant.endswith("+rules"),
        "runs": s["runs_scored"],
        "failed": s["failed"],
        "rough": at["reviewed"] < FEW_REVIEWED,
        "precision": at["precision"],
        "precision_ci95": at["ci95"],
        "found": found,
        "found_ci95": [reviews * p for p in at["ci95"]],
        "multiple": found / random_found,
        "reviews_per_failure": reviews / found,
        "random_reviews_for_same": found / COST_FAILURE_RATE,
        "jev_usd": jev,
        "jev_usd_per_extra_failure": jev / (found - random_found) if found > random_found else None,
        "input_tokens_per_run": s["tokens"] / s["runs_scored"],
        "roc_auc": s["default"].get("roc_auc"),
        "vs_baseline": gain and {"against": "jev_alone" if variant.endswith("+rules") else "answer_wrong_alone",
                                 "gain": gain["gain"], "ci95": gain["ci95"]},
        "run": meta["run_id"],
    }


def build(runs: list[Path]) -> dict[str, Any]:
    rows: list[dict[str, Any]] = []
    first: dict[str, Any] = {}
    for folder in runs:
        summary = json.loads((folder / "summary.json").read_text())
        meta = summary["meta"]
        first = first or meta
        for ds, s in summary["datasets"].items():
            for part, group in (("all", s), ("detectable", summary.get("detectable", {}).get(ds))):
                if group and (r := _row(ds, part, group, meta)):
                    rows.append(r)
    budget = first["config"].get("review_budget", 0.05)
    reviews = round(COST_TRACES * budget)
    return {
        "schema": SCHEMA,
        "date": f"{first['run_id'][:4]}-{first['run_id'][4:6]}-{first['run_id'][6:8]}",
        "jev_model": first["jev_models"][0],
        "source": first["source"]["base_sha"],
        "usd_per_million_input_tokens": first["config"]["cost"]["usd_per_million_input_tokens"],
        "scenario": {"traces": COST_TRACES, "failure_rate": COST_FAILURE_RATE, "review_budget": budget,
                     "reviews": reviews, "random_found": reviews * COST_FAILURE_RATE,
                     "random_reviews_per_failure": 1 / COST_FAILURE_RATE},
        "datasets": DATASETS,
        "results": rows,
    }


def write(runs: list[Path], out: Path) -> dict[str, Any]:
    data = build(runs)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
    return data
