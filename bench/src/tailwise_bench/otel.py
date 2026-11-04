"""OTLP/JSON for Tailwise's POST /v1/otlp/traces:score, one request per run.

A run becomes the spans an OpenInference-instrumented agent emits for the
parts Tailwise reads: an AGENT root span with the question as input.value and
the final answer as output.value, an LLM span whose system message is the
agent's system prompt, and a TOOL span per tool call with its arguments,
output and status. TRAIL runs are sent as their own spans.
"""

from __future__ import annotations

import hashlib
import json
import re
from datetime import datetime
from typing import Any

from .types import Run

# Tailwise's own limits when it renders the state (internal/trace/slice.go).
TAILWISE_MAX_EVIDENCE = 3
TAILWISE_MAX_EVIDENCE_CHARS = 1500
TAILWISE_MAX_SYSTEM_PROMPT_CHARS = 8000


def hex_id(*parts: str, size: int = 16) -> str:
    """A stable, nonzero hex ID of size bytes from parts."""
    digest = hashlib.sha256("\x1f".join(parts).encode()).hexdigest()[: size * 2]
    return digest if set(digest) != {"0"} else "1" + digest[1:]


def attr(key: str, value: Any) -> dict[str, Any]:
    if isinstance(value, bool):
        return {"key": key, "value": {"boolValue": value}}
    if isinstance(value, int):
        return {"key": key, "value": {"intValue": str(value)}}
    if isinstance(value, float):
        return {"key": key, "value": {"doubleValue": value}}
    if not isinstance(value, str):
        value = json.dumps(value, ensure_ascii=False, default=str)
    return {"key": key, "value": {"stringValue": value}}


def request(spans: list[dict[str, Any]], resource: dict[str, Any] | None = None) -> dict[str, Any]:
    """An ExportTraceServiceRequest holding spans."""
    res = [attr(k, v) for k, v in (resource or {}).items()]
    return {"resourceSpans": [{
        "resource": {"attributes": res},
        "scopeSpans": [{"scope": {"name": "tailwise-bench"}, "spans": spans}],
    }]}


def run_trace(run: Run) -> tuple[dict[str, Any], str]:
    """The run as OTLP spans, and the state Tailwise will build from them
    (for token estimates)."""
    if run.otlp_spans is not None:
        return native_trace(run)
    root_id = hex_id("root", run.trace_id, size=8)
    root_attrs = [
        attr("openinference.span.kind", "AGENT"),
        attr("input.value", run.question),
        attr("output.value", run.final_answer),
    ]
    if run.agent:
        root_attrs.append(attr("agent.name", run.agent))
    end = 10_000 * (len(run.tool_calls) + 2)
    spans = [{"traceId": run.trace_id, "spanId": root_id, "name": run.agent or "agent_run",
              "startTimeUnixNano": "1000", "endTimeUnixNano": str(end), "attributes": root_attrs}]
    if run.system_prompt:
        # Where an OpenInference instrumentor records the agent's instructions:
        # the system message of its first LLM call.
        spans.append({
            "traceId": run.trace_id, "spanId": hex_id("llm", run.trace_id, size=8), "parentSpanId": root_id,
            "name": "llm", "startTimeUnixNano": "1500", "endTimeUnixNano": "1900",
            "attributes": [attr("openinference.span.kind", "LLM"),
                           attr("llm.input_messages.0.message.role", "system"),
                           attr("llm.input_messages.0.message.content", run.system_prompt),
                           attr("llm.input_messages.1.message.role", "user"),
                           attr("llm.input_messages.1.message.content", run.question)],
        })
    for i, c in enumerate(run.tool_calls):
        start = 2000 + 10_000 * i
        a = [attr("openinference.span.kind", "TOOL"), attr("tool.name", c.name),
             attr("input.value", c.args if isinstance(c.args, str) else json.dumps(c.args, ensure_ascii=False)),
             attr("output.value", c.output)]
        span: dict[str, Any] = {
            "traceId": run.trace_id, "spanId": hex_id("tool", run.trace_id, str(i), size=8), "parentSpanId": root_id,
            "name": c.name, "startTimeUnixNano": str(start), "endTimeUnixNano": str(start + 5000), "attributes": a,
        }
        if c.error:
            span["status"] = {"code": 2, "message": "tool error"}
        spans.append(span)
    return request(spans), estimate_state(spans)


# --- TRAIL's own spans --------------------------------------------------------

_DURATION = re.compile(r"^P(?:(?P<d>\d+)D)?(?:T(?:(?P<h>\d+)H)?(?:(?P<m>\d+)M)?(?:(?P<s>[\d.]+)S)?)?$")
_STATUS = {"ok": 1, "error": 2}


def iso_nanos(ts: str) -> int:
    dt = datetime.fromisoformat(ts.replace("Z", "+00:00"))
    return int(dt.timestamp()) * 1_000_000_000 + dt.microsecond * 1000


def duration_nanos(d: str | None) -> int:
    m = _DURATION.match(d or "")
    if not m:
        return 0
    days, hours, mins = (int(m.group(g) or 0) for g in ("d", "h", "m"))
    secs = float(m.group("s") or 0)
    return int(((days * 24 + hours) * 60 + mins) * 60 * 1e9 + secs * 1e9)


def otlp_span(span: dict[str, Any], parent: str | None) -> dict[str, Any]:
    """One TRAIL span, without its children, as an OTLP/JSON span."""
    start = iso_nanos(span["timestamp"])
    out: dict[str, Any] = {
        "traceId": span["trace_id"],
        "spanId": span["span_id"],
        "name": span.get("span_name") or "span",
        "startTimeUnixNano": str(start),
        "endTimeUnixNano": str(start + duration_nanos(span.get("duration"))),
        "attributes": [attr(k, v) for k, v in (span.get("span_attributes") or {}).items() if v is not None],
    }
    if parent:
        out["parentSpanId"] = parent
    code = _STATUS.get(str(span.get("status_code", "")).lower())
    if code:
        out["status"] = {"code": code, "message": span.get("status_message") or ""}
    return out


def native_trace(run: Run) -> tuple[dict[str, Any], str]:
    """A TRAIL run's own spans.

    TRAIL's top span (`main`) carries no input or output, so Tailwise, which
    reads the question and answer from the root, would find nothing to score.
    The outermost AGENT span is made a root (its parent cleared) so Tailwise
    picks it: the AGENT kind wins among parentless spans. Nothing else changes.
    """
    assert run.otlp_spans is not None
    spans = run.otlp_spans
    agents = [s for s in spans if _value(s, "openinference.span.kind") == "AGENT"]
    if agents:
        by_id = {s["spanId"]: s for s in spans}
        outer = min(agents, key=lambda s: _depth(s, by_id))
        spans = [({k: v for k, v in s.items() if k != "parentSpanId"} if s is outer else s) for s in spans]
    return request(spans), estimate_state(spans)


def _depth(span: dict[str, Any], by_id: dict[str, dict[str, Any]]) -> int:
    depth = 0
    while span.get("parentSpanId") in by_id and depth < 10_000:
        span = by_id[span["parentSpanId"]]
        depth += 1
    return depth


def _value(span: dict[str, Any], key: str) -> str:
    for a in span["attributes"]:
        if a["key"] == key:
            return str(next(iter(a["value"].values())))
    return ""


def estimate_state(spans: list[dict[str, Any]]) -> str:
    """Roughly what Tailwise's trace.BuildState renders from the spans: the
    root's question and answer, one line per tool call, at most three evidence
    paragraphs from successful tool outputs. Used only to count tokens."""
    roots = [s for s in spans if "parentSpanId" not in s]
    root = next((s for s in roots if _value(s, "openinference.span.kind") == "AGENT"), roots[0] if roots else spans[0])
    tools = sorted((s for s in spans if _value(s, "openinference.span.kind") == "TOOL"),
                   key=lambda s: int(s["startTimeUnixNano"]))
    lines = [f"{i + 1}. {_value(s, 'tool.name') or s['name']}({_value(s, 'input.value')}) -> "
             f"{'error' if s.get('status', {}).get('code') == 2 else 'ok'}" for i, s in enumerate(tools)]
    evidence = [p[:TAILWISE_MAX_EVIDENCE_CHARS] for s in tools if s.get("status", {}).get("code") != 2
                for p in _value(s, "output.value").split("\n\n") if p.strip()][:TAILWISE_MAX_EVIDENCE]
    ev = [f"[{i + 1}] {e}" for i, e in enumerate(evidence)]
    llm = next((s for s in spans if _value(s, "openinference.span.kind") == "LLM"
                and _value(s, "llm.input_messages.0.message.role") == "system"), None)
    system = _value(llm, "llm.input_messages.0.message.content")[:TAILWISE_MAX_SYSTEM_PROMPT_CHARS] if llm else ""
    prefix = f"SYSTEM PROMPT:\n{system}\n\n" if system else ""
    return (f"{prefix}USER QUESTION:\n{_value(root, 'input.value')}\n\nTOOL CALLS:\n{chr(10).join(lines) or '(none)'}"
            f"\n\nEVIDENCE:\n{chr(10).join(ev) or '(none)'}\n\nFINAL ANSWER:\n{_value(root, 'output.value')}")
