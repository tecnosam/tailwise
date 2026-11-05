"""The run every loader produces and every later stage reads."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class ToolCall:
    name: str
    args: Any  # a dict when the agent passed structured arguments, else a string
    output: str
    error: bool = False


@dataclass
class Run:
    """One agent run: what Tailwise sees, and whether it actually failed."""

    dataset: str  # tau, swe, trail
    subset: str  # e.g. retail/gpt-4o, swe-agent-llama-70b, gaia
    run_id: str  # the dataset's own id for the run
    trace_id: str  # 32 hex characters
    failed: bool  # the label: the run did not do its job
    system_prompt: str = ""
    question: str = ""
    tool_calls: list[ToolCall] = field(default_factory=list)
    final_answer: str = ""
    # The user's and agent's turns, (role, text), when the dataset has them:
    # sent as the history of the agent's last LLM call.
    messages: list[tuple[str, str]] = field(default_factory=list)
    agent: str = ""
    # TRAIL only: the run's own spans as OTLP/JSON, sent instead of building them.
    otlp_spans: list[dict[str, Any]] | None = None
    meta: dict[str, Any] = field(default_factory=dict)
