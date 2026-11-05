"""τ-bench (sierra-research/tau-bench): customer-service agents (retail,
airline) talking to a simulated user and calling tools.

The repository's historical_trajectories/ hold every run of gpt-4o and
claude-3.5-sonnet with its reward: 1 when the database ended in the right
state and the agent told the user what it had to, else 0. A run with
reward < 1 failed. Files are downloaded at a pinned commit (MIT licensed).

Each trajectory is OpenAI-style messages: system (the policy), user,
assistant (text or tool_calls), tool (a call's result, "Error: ..." when it
failed). Tailwise sees the policy as the system prompt, the user's messages
as the question, every tool call with its result, and the agent's last
message as the final answer. The user's and agent's turns go on the agent's
last LLM call, as its history and reply.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import httpx

from ..otel import hex_id
from ..types import Run, ToolCall

RAW = "https://raw.githubusercontent.com/sierra-research/tau-bench/{commit}/historical_trajectories/{file}.json"
STOP = "###STOP###"


def download(commit: str, file: str, cache: Path) -> Path:
    path = cache / "tau-bench" / commit / f"{file}.json"
    if not path.exists():
        path.parent.mkdir(parents=True, exist_ok=True)
        r = httpx.get(RAW.format(commit=commit, file=file), timeout=120, follow_redirects=True)
        r.raise_for_status()
        tmp = path.with_suffix(".part")
        tmp.write_bytes(r.content)
        tmp.rename(path)
    return path


def load(commit: str, files: list[str], cache: Path, limit: int | None = None) -> tuple[list[Run], dict[str, Any]]:
    from ..investigate.audit import trace_detectable  # audit imports this module

    runs: list[Run] = []
    raw: list[tuple[str, dict[str, Any]]] = []
    for i, file in enumerate(files):
        rows = json.loads(download(commit, file, cache).read_text())
        n = None if limit is None else limit // len(files) + (1 if i < limit % len(files) else 0)
        runs += [parse(file, row) for row in rows[:n]]
        raw += [(file.rpartition("-")[2], row) for row in rows[:n]]
    for run, labels in zip(runs, trace_detectable(raw), strict=True):
        run.meta.update(labels)
    return runs, {"runs": len(runs), "failed": sum(r.failed for r in runs), "commit": commit,
                  "trace_detectable": sum(r.meta["trace_detectable"] for r in runs),
                  "trace_detectable_failed": sum(r.failed and r.meta["trace_detectable"] for r in runs)}


def pair_results(msgs: list[dict[str, Any]]) -> dict[int, tuple[dict[str, Any], int]]:
    """Each tool result, by the position of the call it answers (in the order
    calls were made): {call index: (tool message, its message index)}.

    Results follow their calls in order. A result's tool_call_id picks its
    call only when exactly one waiting call has that id: the claude-3.5-sonnet
    logs give every call the id "0", and the gpt-4o logs reuse ids on retries,
    so pairing by id alone gives calls the wrong results.
    """
    waiting: list[tuple[int, str]] = []  # (call index, id) of calls not yet answered
    out: dict[int, tuple[dict[str, Any], int]] = {}
    n = 0
    for mi, m in enumerate(msgs):
        if m.get("role") == "assistant":
            for tc in m.get("tool_calls") or []:
                waiting.append((n, str(tc.get("id"))))
                n += 1
        elif m.get("role") == "tool" and waiting:
            same = [w for w in waiting if w[1] == str(m.get("tool_call_id"))]
            pick = same[0] if len(same) == 1 else waiting[0]
            waiting.remove(pick)
            out[pick[0]] = (m, mi)
    return out


def parse(file: str, row: dict[str, Any]) -> Run:
    for key in ("task_id", "reward", "traj"):
        if key not in row:
            raise ValueError(f"τ-bench {file}: run without {key!r} (keys {sorted(row)})")
    model, _, domain = file.rpartition("-")
    run_id = f"{file}:{row['task_id']}:{row.get('trial', 0)}"
    msgs = row["traj"]
    results = pair_results(msgs)

    user_turns: list[str] = []
    turns: list[tuple[str, str]] = []
    calls: list[ToolCall] = []
    final = ""
    system = next((m.get("content") or "" for m in msgs if m.get("role") == "system"), "")
    for m in msgs:
        role, content = m.get("role"), m.get("content") or ""
        if role == "user":
            text = content.replace(STOP, "").strip()
            if text:
                user_turns.append(text)
                turns.append(("user", text))
        elif role == "assistant":
            if content.strip():
                final = content
                turns.append(("assistant", content))
            for tc in m.get("tool_calls") or []:
                fn = tc.get("function") or {}
                args: Any = fn.get("arguments") or {}
                if isinstance(args, str):  # OpenAI sends a JSON string; some files hold the object
                    try:
                        args = json.loads(args or "{}")
                    except json.JSONDecodeError:
                        pass
                out = str(results[len(calls)][0].get("content") or "") if len(calls) in results else ""
                calls.append(ToolCall(name=fn.get("name") or "tool", args=args, output=out,
                                      error=out.startswith("Error")))
    return Run(
        dataset="tau", subset=f"{domain}/{model}", run_id=run_id, trace_id=hex_id("tau", run_id),
        failed=float(row["reward"]) < 1, system_prompt=system, question="\n\n".join(user_turns),
        tool_calls=calls, final_answer=final, messages=turns,
        meta={"domain": domain, "model": model, "task_id": row["task_id"], "trial": row.get("trial", 0),
              "reward": row["reward"]},
    )
