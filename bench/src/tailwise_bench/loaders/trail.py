"""TRAIL (PatronusAI/TRAIL): real agent traces (GAIA, SWE-bench) with every
error annotated on the span where it happened, with a category and impact.

TRAIL labels errors, not outcomes, so a run counts as failed when it has at
least one HIGH-impact error: the closest it comes to "this run went wrong".
Nearly every trace has some error, so "any error" would leave almost nothing
to tell apart.

Gated on Hugging Face and licensed for evaluation only: the data is
downloaded into the git-ignored cache and never written anywhere else, never
used to train or tune anything.

Each row holds `trace` (a JSON string: {"trace_id", "spans": [...]}, the
spans nested under "child_spans") and `labels` (a JSON string: {"errors":
[{"location": span_id, "category", "impact", ...}], "scores": [...]}).
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pyarrow.parquet as pq
from huggingface_hub import HfApi, hf_hub_download
from huggingface_hub.errors import GatedRepoError, HfHubHTTPError

from ..otel import iso_nanos, otlp_span
from ..types import Run


class SchemaError(ValueError):
    pass


def download(repo: str, splits: list[str], cache: Path) -> dict[str, Path]:
    """The parquet file of each split, downloaded once."""
    try:
        files = HfApi().list_repo_files(repo, repo_type="dataset")
        out = {}
        for split in splits:
            matches = [f for f in files if f.startswith(f"data/{split}-") and f.endswith(".parquet")]
            if len(matches) != 1:
                raise SchemaError(f"{repo}: expected one parquet file for split {split!r}, found {matches}")
            out[split] = Path(hf_hub_download(repo, matches[0], repo_type="dataset", local_dir=cache / "hf" / repo))
        return out
    except GatedRepoError as e:
        raise SystemExit(
            f"{repo} is gated and this Hugging Face account has no access yet. Accept its terms at "
            f"https://huggingface.co/datasets/{repo}, then set HF_TOKEN or run `hf auth login`."
        ) from e
    except HfHubHTTPError as e:
        if e.response is not None and e.response.status_code in (401, 403):
            raise SystemExit(f"{repo}: access denied (HTTP {e.response.status_code}). Accept the terms at "
                             f"https://huggingface.co/datasets/{repo} and check HF_TOKEN.") from e
        raise


def load(repo: str, splits: list[str], cache: Path, limit: int | None = None) -> tuple[list[Run], dict[str, Any]]:
    files = download(repo, splits, cache)
    runs: list[Run] = []
    for i, split in enumerate(splits):
        rows = pq.read_table(files[split]).to_pylist()
        if rows and not {"trace", "labels"} <= rows[0].keys():
            raise SchemaError(f"{repo}/{split}: expected columns trace and labels, got {sorted(rows[0])}")
        n = None if limit is None else limit // len(splits) + (1 if i < limit % len(splits) else 0)
        runs += [parse(split, json.loads(r["trace"]), json.loads(r["labels"])) for r in rows[:n]]
    return runs, {"runs": len(runs), "failed": sum(r.failed for r in runs),
                  "with_any_error": sum(r.meta["errors"] > 0 for r in runs)}


def parse(split: str, trace: dict[str, Any], labels: dict[str, Any]) -> Run:
    if "spans" not in trace or "trace_id" not in trace:
        raise SchemaError(f"TRAIL trace without spans or trace_id: keys {sorted(trace)}")
    flat: list[tuple[dict[str, Any], str | None]] = []

    def walk(span: dict[str, Any], parent: str | None) -> None:
        flat.append((span, parent))
        for child in span.get("child_spans") or []:
            walk(child, span["span_id"])

    for top in trace["spans"]:
        walk(top, top.get("parent_span_id"))
    flat.sort(key=lambda sp: iso_nanos(sp[0]["timestamp"]))

    errors = labels.get("errors") or []
    impacts = [str(e.get("impact", "")).strip().upper() for e in errors]
    return Run(
        dataset="trail", subset=split, run_id=trace["trace_id"], trace_id=trace["trace_id"],
        failed="HIGH" in impacts, otlp_spans=[otlp_span(s, p) for s, p in flat],
        meta={"errors": len(errors), "high_impact_errors": impacts.count("HIGH"),
              "categories": sorted({str(e.get("category", "")).strip() for e in errors})},
    )
