"""Input-token estimates, for --dry-run and for reserving budget before a call.

TypeSafe does not publish Jev's tokenizer, so these use tiktoken and are
labelled estimates. Real runs bill from the `input_tokens` Jev reports.
"""

from __future__ import annotations

import json
from functools import lru_cache
from pathlib import Path

import tiktoken

# The questions Tailwise sends with every call, unless the bench names a manifest.
BUILTIN_MANIFEST = Path(__file__).resolve().parents[3] / "internal" / "config" / "builtin_manifest.json"


class Estimator:
    def __init__(self, encoding: str, manifest_text: str = "") -> None:
        self._enc = tiktoken.get_encoding(encoding)
        manifest = json.loads(manifest_text or BUILTIN_MANIFEST.read_text())
        # Jev reads the questions as well as the state.
        self.questions_tokens = self.count(json.dumps(manifest.get("questions", manifest)))

    @lru_cache(maxsize=65536)
    def count(self, text: str) -> int:
        return len(self._enc.encode(text, disallowed_special=()))

    def call(self, state: str) -> int:
        return self.count(state) + self.questions_tokens
