"""Every score, on disk, so a rerun or a resumed run costs nothing.

Keyed by (dataset, trace_id, span_id, model, config hash); span_id is
"__trace__" for a whole-trace score. Only real scores are stored: a call
that failed or fell back is retried on the next run.
"""

from __future__ import annotations

import json
import sqlite3
import threading
from pathlib import Path
from typing import Any

TRACE = "__trace__"


class ScoreCache:
    def __init__(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        self._db = sqlite3.connect(path, check_same_thread=False)
        self._lock = threading.Lock()
        self._db.execute(
            "CREATE TABLE IF NOT EXISTS scores (dataset TEXT, trace_id TEXT, span_id TEXT, model TEXT,"
            " config_hash TEXT, result TEXT, PRIMARY KEY (dataset, trace_id, span_id, model, config_hash))"
        )
        self._db.commit()

    def get(self, key: tuple[str, str, str, str, str]) -> dict[str, Any] | None:
        with self._lock:
            row = self._db.execute(
                "SELECT result FROM scores WHERE dataset=? AND trace_id=? AND span_id=? AND model=? AND config_hash=?",
                key,
            ).fetchone()
        return json.loads(row[0]) if row else None

    def put(self, key: tuple[str, str, str, str, str], result: dict[str, Any]) -> None:
        with self._lock:
            self._db.execute("INSERT OR REPLACE INTO scores VALUES (?, ?, ?, ?, ?, ?)", (*key, json.dumps(result)))
            self._db.commit()

    def close(self) -> None:
        self._db.close()
