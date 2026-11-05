"""Tailwise's heuristic rules, run over the requests the harness sends.

Rules do not change what Jev is asked, so they are not part of the score
cache: bench/rulecheck (Go, Tailwise's own rules and span assembly) checks
each run's request, and analyze combines the verdicts with the cached
answers as internal/pipeline does. A rule set is measured at no cost, on the
same answers as the run without it.
"""

from __future__ import annotations

import hashlib
import json
import subprocess
from pathlib import Path
from typing import Any

from .otel import run_trace
from .types import Run

Verdicts = dict[str, list[dict[str, Any]]]  # by trace_id: every rule's verdict


def files(rules_dir: Path, datasets: list[str]) -> dict[str, Path]:
    """Each dataset's rule file, <dataset>.yaml in rules_dir, when it has one."""
    return {ds: rules_dir / f"{ds}.yaml" for ds in datasets if (rules_dir / f"{ds}.yaml").is_file()}


def digest(paths: dict[str, Path]) -> dict[str, str]:
    return {ds: hashlib.sha256(p.read_bytes()).hexdigest()[:16] for ds, p in sorted(paths.items())}


def check(runs: list[Run], paths: dict[str, Path], repo: Path) -> Verdicts:
    out: Verdicts = {}
    for ds, path in sorted(paths.items()):
        lines = "".join(json.dumps({"id": r.trace_id, "request": run_trace(r)[0]}) + "\n"
                        for r in runs if r.dataset == ds)
        proc = subprocess.run(["go", "run", "./bench/rulecheck", "-rules", str(path.resolve())], cwd=repo,
                              input=lines, capture_output=True, text=True)
        if proc.returncode:
            raise RuntimeError(f"rulecheck {path}: {proc.stderr.strip()}")
        for line in proc.stdout.splitlines():
            v = json.loads(line)
            out[v["id"]] = v["rules"] or []
    return out
