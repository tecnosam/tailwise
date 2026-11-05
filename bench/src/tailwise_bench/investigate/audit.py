"""Plain-code checks on τ-bench runs, no model involved.

- grounding: does every argument of a write call come from something the
  agent had seen before making it: the user's messages, the policy (system
  prompt), or an earlier tool result?
- gold: do the write calls match the task's ground-truth actions?
- visibility: which tool results reached the model in a captured request?
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from typing import Any

from ..loaders import tau

READ_PREFIXES = ("get_", "find_", "list_", "search_", "calculate", "think")
NOT_ACTIONS = {"transfer_to_human_agents"}  # hands over; its only argument is a free-text summary
SKIP_KEYS = {"reason", "summary"}  # enums and free text the agent writes itself
ENUM_KEYS = {"cabin", "flight_type", "insurance"}
COUNT_KEYS = {"total_baggages", "nonfree_baggages"}
ITEM_WRITES = {"exchange_delivered_order_items", "modify_pending_order_items"}
TARGET_KEYS = ("order_id", "reservation_id", "user_id")
# An id or text argument found nowhere the agent could have read it makes a
# write ungrounded. A date or amount not found word for word is counted as
# derived instead: agents compute them ("one day later", a fare total).
CHECKED_KINDS = ("id", "text")
MONTHS = ["january", "february", "march", "april", "may", "june", "july", "august", "september", "october",
          "november", "december"]


def is_write(name: str) -> bool:
    return not name.startswith(READ_PREFIXES) and name not in NOT_ACTIONS


# --- grounding -----------------------------------------------------------------

@dataclass
class Leaf:
    path: str
    value: Any
    kind: str  # id, date, text, amount, enum, count
    source: str = ""  # user, system, tool:<call index>, or "" when not found
    tool_index: int | None = None
    seen_in_tool: bool = False  # in any earlier tool result, whatever the source


@dataclass
class WriteCall:
    call_index: int
    name: str
    args: dict[str, Any]
    message_index: int
    leaves: list[Leaf] = field(default_factory=list)
    output: str = ""
    # How far back the furthest tool result it relied on was: tool calls and
    # characters of conversation in between; None when no leaf came from a result.
    calls_between: int | None = None
    chars_between: int | None = None

    @property
    def ungrounded(self) -> list[Leaf]:
        return [lf for lf in self.leaves if lf.kind in CHECKED_KINDS and not lf.source]

    @property
    def derived(self) -> list[Leaf]:
        return [lf for lf in self.leaves if lf.kind in ("amount", "date") and not lf.source]

    @property
    def acted_without_lookup(self) -> bool:
        """Its target (order, reservation, user) was never in a tool result
        before the call: the agent acted on it without looking it up."""
        return any(lf.path in TARGET_KEYS and not lf.seen_in_tool for lf in self.leaves)


def leaves(obj: Any, path: str = "") -> list[tuple[str, Any]]:
    if isinstance(obj, dict):
        return [x for k, v in obj.items() for x in leaves(v, f"{path}.{k}" if path else k)]
    if isinstance(obj, list):
        return [x for i, v in enumerate(obj) for x in leaves(v, f"{path}[{i}]")]
    return [(path, obj)]


def kind_of(path: str, value: Any) -> str:
    key = re.sub(r"\[\d+\]", "", path).split(".")[-1]
    if isinstance(value, bool) or value is None or key in SKIP_KEYS:
        return "skip"
    if isinstance(value, int | float):
        return "count" if key in COUNT_KEYS or (float(value).is_integer() and abs(value) < 10) else "amount"
    s = str(value).strip()
    if not s:
        return "skip"
    if key in ENUM_KEYS or (re.fullmatch(r"[a-z_]+", s) and len(s) <= 20):
        return "enum"
    if re.fullmatch(r"\d{4}-\d{2}-\d{2}", s):
        return "date"
    if re.search(r"\d", s):
        return "id"
    return "text"


def _token_re(value: str) -> re.Pattern[str]:
    return re.compile(r"(?<![0-9a-z])" + re.escape(value) + r"(?![0-9a-z])")


def number_forms(v: float) -> list[str]:
    forms = {f"{v:.2f}".rstrip("0").rstrip("."), f"{v:.2f}", f"{v:,.2f}"}
    if float(v).is_integer():
        forms |= {str(int(v)), f"{int(v):,}"}
    return sorted(forms)


def found_in(kind: str, value: Any, text: str, natural: bool) -> bool:
    """Whether value appears in text (lower-cased). natural allows the forms a
    person would type (dates as "May 20")."""
    if kind == "id":
        return bool(_token_re(str(value).strip().lower().lstrip("#")).search(text))
    if kind == "text":
        return " ".join(str(value).lower().split()) in " ".join(text.split())
    if kind == "amount":
        return any(_token_re(f).search(text) for f in number_forms(float(value)))
    if kind == "date":
        s = str(value)
        if s in text:
            return True
        if natural:
            y, m, d = (int(x) for x in s.split("-"))
            month = MONTHS[m - 1]
            for form in (f"{month} {d}", f"{d} {month}", f"{month[:3]} {d}", f"{d} {month[:3]}", f"{m}/{d}",
                         f"{m:02d}/{d:02d}", f"{month} {d}th", f"{month} {d}st", f"{month} {d}nd", f"{month} {d}rd"):
                if _token_re(form).search(text):
                    return True
        return False
    return True


def ground(traj: list[dict[str, Any]], system_prompt: str) -> list[WriteCall]:
    """Every write call in the run, each argument traced to where the agent
    could have taken it from before the call."""
    user_text = ""
    outputs: list[tuple[int, str]] = []  # (call index, lower-cased result) in the order results arrived
    result_msg: dict[int, int] = {}  # call index -> message index of its result
    # characters of conversation before each message, for distances
    before = [0]
    for m in traj:
        before.append(before[-1] + len(m.get("content") or "") + len(json.dumps(m.get("tool_calls") or "")))
    writes: list[WriteCall] = []
    paired = tau.pair_results(traj)
    call_of_msg = {mi: ci for ci, (_, mi) in paired.items()}
    system = system_prompt.lower()
    n = 0
    for mi, m in enumerate(traj):
        role = m.get("role")
        if role == "user":
            user_text += "\n" + (m.get("content") or "").lower()
        elif role == "tool":
            ci = call_of_msg.get(mi, -1)
            outputs.append((ci, (m.get("content") or "").lower()))
            result_msg[ci] = mi
        elif role == "assistant":
            for tc in m.get("tool_calls") or []:
                fn = tc.get("function") or {}
                args: Any = fn.get("arguments") or {}
                if isinstance(args, str):
                    try:
                        args = json.loads(args or "{}")
                    except json.JSONDecodeError:
                        args = {}
                name = fn.get("name") or ""
                if is_write(name) and isinstance(args, dict):
                    w = WriteCall(n, name, args, mi, output=str(paired[n][0].get("content") or "") if n in paired else "")
                    for path, value in leaves(args):
                        lf = Leaf(path, value, kind_of(path, value))
                        if lf.kind in ("id", "date", "text", "amount"):
                            lf.seen_in_tool = any(found_in(lf.kind, value, out, natural=False) for _, out in outputs)
                            if found_in(lf.kind, value, user_text, natural=True):
                                lf.source = "user"
                            elif found_in(lf.kind, value, system, natural=False):
                                lf.source = "system"
                            else:
                                for ci, out in reversed(outputs):
                                    if found_in(lf.kind, value, out, natural=False):
                                        lf.source, lf.tool_index = f"tool:{ci}", ci
                                        break
                        w.leaves.append(lf)
                    sources = [lf.tool_index for lf in w.leaves if lf.tool_index is not None]
                    if sources:
                        furthest = min(sources)
                        w.calls_between = n - furthest - 1
                        w.chars_between = before[mi] - before[result_msg[furthest] + 1]
                    writes.append(w)
                n += 1
    return writes


# --- gold ---------------------------------------------------------------------

def norm(v: Any) -> Any:
    if isinstance(v, dict):
        return {k: norm(x) for k, x in v.items()}
    if isinstance(v, list):
        items = [norm(x) for x in v]
        return sorted(items, key=json.dumps) if all(not isinstance(x, dict | list) for x in items) else items
    if isinstance(v, bool) or v is None:
        return v
    if isinstance(v, int | float):
        return round(float(v), 2)
    return str(v).strip()


def target(args: dict[str, Any]) -> tuple[str, Any] | None:
    for k in TARGET_KEYS:
        if k in args:
            return k, norm(args[k])
    return None


@dataclass
class GoldResult:
    rows: list[dict[str, Any]]  # one per run write call and per missed gold action

    def has(self, category: str) -> bool:
        return any(r["category"] == category for r in self.rows)


def compare_gold(writes: list[WriteCall], gold: list[dict[str, Any]], traj: list[dict[str, Any]] | None = None) -> GoldResult:
    gold_writes = [g for g in gold if is_write(g.get("name", ""))]
    left = list(range(len(gold_writes)))
    rows: list[dict[str, Any]] = []
    pending: list[WriteCall] = []
    for w in writes:
        hit = next((i for i in left if gold_writes[i]["name"] == w.name
                    and norm(gold_writes[i].get("kwargs", {})) == norm(w.args)), None)
        if hit is not None:
            left.remove(hit)
            rows.append({"call_index": w.call_index, "tool": w.name, "category": "correct", "diff": "",
                         "grounded_wrong": False, "variant_mixup": False, "mixup_detectable_from_trace": None})
        else:
            pending.append(w)
    for w in pending:
        t = target(w.args)
        hit = next((i for i in left if gold_writes[i]["name"] == w.name
                    and target(gold_writes[i].get("kwargs", {})) == t), None)
        if hit is None:
            rows.append({"call_index": w.call_index, "tool": w.name, "category": "extra", "diff": "",
                         "grounded_wrong": False, "variant_mixup": False, "mixup_detectable_from_trace": None})
            continue
        left.remove(hit)
        g = gold_writes[hit].get("kwargs", {})
        traj = traj or []
        keys = sorted(k for k in set(g) | set(w.args) if norm(g.get(k)) != norm(w.args.get(k)))
        bad = [lf for lf in w.leaves if lf.path.split(".")[0].split("[")[0] in keys]
        grounded = all(lf.source or lf.kind not in CHECKED_KINDS for lf in bad)
        detectable = None
        if w.name in ITEM_WRITES and "new_item_ids" in keys:
            detectable = mixup_detectable(traj, [str(x) for x in w.args.get("new_item_ids") or []],
                                          [str(x) for x in g.get("new_item_ids") or []])
        rows.append({
            "call_index": w.call_index, "tool": w.name, "category": "wrong_args",
            "mixup_detectable_from_trace": detectable,
            "diff": "; ".join(f"{k}: {json.dumps(w.args.get(k))} vs gold {json.dumps(g.get(k))}" for k in keys),
            "grounded_wrong": grounded,
            "variant_mixup": w.name in ITEM_WRITES and "new_item_ids" in keys and grounded,
        })
    for i in left:
        rows.append({"call_index": None, "tool": gold_writes[i]["name"], "category": "missed",
                     "diff": json.dumps(gold_writes[i].get("kwargs", {}))[:300],
                     "grounded_wrong": False, "variant_mixup": False, "mixup_detectable_from_trace": None})
    return GoldResult(rows)


# --- what reached the model -------------------------------------------------------

OUT_KEYS = ["result", "output", "content", "text"]


def message_text(v: str) -> str:
    """Tailwise's messageText for a tool output (internal/otlp/convert.go)."""
    t = v.strip()
    if not t:
        return ""
    if t[0] == '"':
        try:
            s = json.loads(t)
            return s if isinstance(s, str) else v
        except json.JSONDecodeError:
            return v
    if t[0] == "{":
        try:
            o = json.loads(t)
        except json.JSONDecodeError:
            return v
        if isinstance(o.get("response"), dict):
            o = o["response"]
        for k in OUT_KEYS:
            if isinstance(o.get(k), str):
                return str(o[k])
    return v


def section(state: str, name: str) -> str:
    heads = ["SYSTEM PROMPT", "USER QUESTION", "TOOL CALLS", "EVIDENCE", "FINAL ANSWER"]
    start = state.find(name + ":\n")
    if start < 0:
        return ""
    start += len(name) + 2
    ends = [state.find("\n\n" + h + ":\n", start) for h in heads[heads.index(name) + 1:]]
    ends = [e for e in ends if e >= 0]
    return state[start:min(ends)] if ends else state[start:]


def visibility(state: str, outputs: list[str]) -> list[str]:
    """For each tool call's output: "full", "partial" or "none", by what the
    captured state shows of it, on its tool-call line or in the evidence."""
    shown = ["none"] * len(outputs)
    texts = [message_text(o).strip() for o in outputs]
    calls = section(state, "TOOL CALLS")
    blocks = re.split(r"(?m)^(?=\d+\. )", calls)
    for block in blocks:
        m = re.match(r"(\d+)\. ", block)
        if not m:
            continue
        i = int(m.group(1)) - 1
        if not 0 <= i < len(outputs) or not texts[i]:
            continue
        first, _, more = block.partition("\n")
        result = ""
        for status in ("ok", "error"):
            mark = f") -> {status}: "
            if mark in first:
                result = first.split(mark, 1)[1]
        if not result and more.startswith("   "):
            result = "\n".join(line[3:] for line in more.rstrip("\n").split("\n"))
        if result:
            shown[i] = "full" if result.strip() == texts[i] else "partial"
    items = re.split(r"(?m)^\[\d+\] ", section(state, "EVIDENCE"))[1:]
    for item in items:
        item = item.rstrip("\n").strip()
        if not item or item == "(none)":
            continue
        for i, t in enumerate(texts):
            if shown[i] == "none" and item[:300] in t:
                shown[i] = "full" if item == t else "partial"
                break
    return shown


# --- variant mix-ups: could the trace show them? --------------------------------

def variant_options(traj: list[dict[str, Any]]) -> dict[str, dict[str, Any]]:
    """item_id -> options, from every product lookup in the run."""
    out: dict[str, dict[str, Any]] = {}
    for m in traj:
        if m.get("role") != "tool":
            continue
        try:
            o = json.loads(m.get("content") or "")
        except json.JSONDecodeError:
            continue
        if isinstance(o, dict) and isinstance(o.get("variants"), dict):
            for k, v in o["variants"].items():
                if isinstance(v, dict):
                    out[k] = v.get("options") or {}
    return out


def mixup_detectable(traj: list[dict[str, Any]], chosen: list[str], gold: list[str]) -> bool | None:
    """Whether the user ever stated an option that tells a gold item apart
    from the closest item the agent chose instead. None when the run never
    looked up the variants involved."""
    options = variant_options(traj)
    user = " ".join((m.get("content") or "") for m in traj if m.get("role") == "user").lower()
    verdict: bool | None = None
    for g in gold:
        if g in chosen or g not in options:
            continue
        closest: list[str] | None = None
        for c in chosen:
            if c not in options:
                continue
            diff = [str(v).lower() for k, v in options[g].items() if str(v).lower() != str(options[c].get(k)).lower()]
            if closest is None or len(diff) < len(closest):
                closest = diff
        if closest is not None:
            found = any(v in user for v in closest)
            verdict = found if verdict is None else verdict or found
    return verdict


# --- which failures a trace can show -----------------------------------------------

def trace_detectable(runs: list[tuple[str, dict[str, Any]]]) -> list[dict[str, int]]:
    """For each (domain, τ-bench row): whether its label can be judged from
    the trace alone.

    - gold_consistent_task: every successful run of the task carried out the
      logged gold actions exactly. On the other tasks the gold disagrees with
      how runs were scored, so a run's writes say little about its label.
    - mixup_not_in_trace: the run failed only by choosing the wrong variant of
      an item, and the user never stated the option that tells the gold item
      apart; the gold rests on the simulated user's hidden instructions.
    - trace_detectable: the first and not the second.
    """
    out: list[dict[str, Any]] = []
    for domain, row in runs:
        system = next((m.get("content") or "" for m in row["traj"] if m.get("role") == "system"), "")
        actions = ((row.get("info") or {}).get("task") or {}).get("actions") or []
        gold = compare_gold(ground(row["traj"], system), actions, row["traj"])
        wrong = [g for g in gold.rows if g["category"] != "correct"]
        out.append({"task": f"{domain}:{row['task_id']}", "succeeded": float(row["reward"]) >= 1, "wrong": bool(wrong),
                    "mixup_not_in_trace": bool(wrong) and all(g["mixup_detectable_from_trace"] is False for g in wrong)})
    bad_tasks = {r["task"] for r in out if r["succeeded"] and r["wrong"]}
    return [{"gold_consistent_task": int(r["task"] not in bad_tasks),
             "mixup_not_in_trace": int(not r["succeeded"] and r["mixup_not_in_trace"]),
             "trace_detectable": int(r["task"] not in bad_tasks and not (not r["succeeded"] and r["mixup_not_in_trace"]))}
            for r in out]
