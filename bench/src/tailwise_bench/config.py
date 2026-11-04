"""The benchmark config: bench/config.yaml, typed."""

from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass, field
from functools import cache
from pathlib import Path
from typing import Any

import yaml


# Tailwise's sources that decide what Jev is sent, relative to the repository.
STATE_CODE = [Path("internal/trace/*.go"), Path("internal/otlp/convert.go")]


@dataclass(frozen=True)
class Tailwise:
    start: bool = True
    binary: str = "../bin/tailwise"
    url: str = "http://127.0.0.1:18080"
    envchain: str = "observer-ai"
    model: str = "jev-latest"
    keep_threshold: float = 0.5
    manifest: str = ""
    system_prompt: bool = False
    evidence: str = "grounded"
    evidence_budget: int = 12000


@dataclass(frozen=True)
class Cost:
    usd_per_million_input_tokens: float = 0.042
    budget_usd: float = 3.0
    tokenizer: str = "cl100k_base"

    def usd(self, tokens: int) -> float:
        return tokens * self.usd_per_million_input_tokens / 1_000_000


@dataclass(frozen=True)
class Client:
    concurrency: int = 8
    requests_per_second: float = 8.0
    max_attempts: int = 6
    backoff_seconds: float = 1.0
    timeout_seconds: float = 120.0


@dataclass(frozen=True)
class Datasets:
    tau: dict[str, Any] = field(default_factory=lambda: {
        "commit": "59a200c6d575d595120f1cb70fea53cef0632f6b",
        "files": ["gpt-4o-retail", "gpt-4o-airline", "sonnet-35-new-retail", "sonnet-35-new-airline"],
    })
    swe: dict[str, Any] = field(default_factory=lambda: {
        "repo": "nebius/SWE-agent-trajectories", "first_shard": 0, "shards": 1, "shards_total": 12, "sample": 1000,
    })
    trail: dict[str, Any] = field(default_factory=lambda: {
        "repo": "PatronusAI/TRAIL", "splits": ["gaia", "swe_bench"],
    })


@dataclass(frozen=True)
class Config:
    path: Path
    seed: int = 0
    random_seeds: int = 100
    budgets: tuple[float, ...] = (0.01, 0.05, 0.10, 0.20, 0.30, 0.50)
    reference_failure_rates: tuple[float, ...] = (0.02, 0.05, 0.10, 0.20)
    tailwise: Tailwise = Tailwise()
    cost: Cost = Cost()
    client: Client = Client()
    datasets: Datasets = Datasets()
    cache_dir: str = ".cache"
    results_dir: str = "results"

    @property
    def base(self) -> Path:
        return self.path.parent

    def resolve(self, p: str) -> Path:
        path = Path(p)
        return path if path.is_absolute() else (self.base / path).resolve()

    @property
    def manifest_text(self) -> str:
        """The manifest's content, "" for the built-in questions."""
        if not self.tailwise.manifest:
            return ""
        return self.resolve(self.tailwise.manifest).read_text()

    @property
    def variant(self) -> str:
        return Path(self.tailwise.manifest).stem if self.tailwise.manifest else "builtin"

    def score_hash(self) -> str:
        """Hash of everything that changes what Jev is sent or how it is read,
        including the Tailwise code that turns spans into Jev's state."""
        relevant = {"model": self.tailwise.model, "keep_threshold": self.tailwise.keep_threshold,
                    "manifest": self.manifest_text, "system_prompt": self.tailwise.system_prompt,
                    "evidence": self.tailwise.evidence,
                    "evidence_budget": self.tailwise.evidence_budget if self.tailwise.evidence == "grounded" else None,
                    "state_code": self.state_code_hash()}
        return hashlib.sha256(json.dumps(relevant, sort_keys=True).encode()).hexdigest()[:16]

    def state_code_hash(self) -> str:
        """Hash of the Tailwise sources that build the state from spans, so a
        change to them re-scores instead of reusing the cache."""
        return _state_code_hash(self.base.parent)


    def to_dict(self) -> dict[str, Any]:
        d = asdict(self)
        d["path"] = str(self.path)
        d["score_hash"] = self.score_hash()
        return d


@cache
def _state_code_hash(repo: Path) -> str:
    """Read once per run, so the hash cannot change while a run is scoring."""
    h = hashlib.sha256()
    for path in sorted(STATE_CODE, key=str):
        for f in sorted((repo / path.parent).glob(path.name)):
            if not f.name.endswith("_test.go"):
                h.update(f.name.encode() + f.read_bytes())
    return h.hexdigest()[:16]


def load(path: Path) -> Config:
    raw = yaml.safe_load(path.read_text()) or {}
    known = {"seed", "random_seeds", "budgets", "reference_failure_rates", "tailwise", "cost", "client",
             "datasets", "cache_dir", "results_dir"}
    if unknown := set(raw) - known:
        raise ValueError(f"{path}: unknown keys {sorted(unknown)}")
    defaults = Datasets()
    ds = raw.get("datasets", {})
    cfg = Config(
        path=path.resolve(),
        seed=int(raw.get("seed", 0)),
        random_seeds=int(raw.get("random_seeds", 100)),
        budgets=tuple(float(b) for b in raw.get("budgets", Config.budgets)),
        reference_failure_rates=tuple(float(r) for r in raw.get("reference_failure_rates", Config.reference_failure_rates)),
        tailwise=Tailwise(**raw.get("tailwise", {})),
        cost=Cost(**raw.get("cost", {})),
        client=Client(**raw.get("client", {})),
        datasets=Datasets(**{k: {**getattr(defaults, k), **v} for k, v in ds.items()}),
        cache_dir=str(raw.get("cache_dir", ".cache")),
        results_dir=str(raw.get("results_dir", "results")),
    )
    if not all(0 < b <= 1 for b in (*cfg.budgets, *cfg.reference_failure_rates)):
        raise ValueError("budgets and reference_failure_rates must be shares in (0, 1]")
    return cfg
