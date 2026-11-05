"""Opt-in experiment: did the agent handle every request the customer made?

Jev answers yes/no and choice questions; it does not write lists. So the
customer's requests are found by fan-out: for each kind of request the domain
allows, one question asks whether the customer made it, and a paired question
asks whether it was fulfilled by an action or explicitly declined by the
agent. A request made (≥ 0.5) and not handled (< 0.5) makes the run
incomplete. One call per run, directly over /v1/systemone, under a hard
budget.
"""

from __future__ import annotations

import csv
import json
import os
import random
from pathlib import Path
from typing import Any

import httpx

from ..budget import BudgetExceeded, BudgetGuard
from ..config import Config
from ..loaders import tau
from ..report import write_csv, write_json
from ..tokens import Estimator
from . import audit
from .run import load_tau

ENDPOINT = "https://api.typesafe.ai/v1/systemone"
MAX_AGENT_CHARS = 600

# The kinds of request each domain's tools can carry out, and the tool for each.
REQUESTS = {
    "retail": {
        "cancel_order": ("cancel a pending order", "cancel_pending_order"),
        "modify_items": ("change items in a pending order (for example another size or color)", "modify_pending_order_items"),
        "modify_order_address": ("change the shipping address of a pending order", "modify_pending_order_address"),
        "modify_payment": ("change the payment method of a pending order", "modify_pending_order_payment"),
        "return_items": ("return delivered items for a refund", "return_delivered_order_items"),
        "exchange_items": ("exchange delivered items for other items", "exchange_delivered_order_items"),
        "modify_user_address": ("change their default address on their account", "modify_user_address"),
    },
    "airline": {
        "book": ("book a new flight reservation", "book_reservation"),
        "change_flights": ("change the flights, dates or cabin of a reservation", "update_reservation_flights"),
        "change_baggage": ("add or change checked bags on a reservation", "update_reservation_baggages"),
        "change_passengers": ("change passenger details on a reservation", "update_reservation_passengers"),
        "cancel_reservation": ("cancel a reservation", "cancel_reservation"),
        "compensation": ("get compensation, a certificate or a refund for a problem", "send_certificate"),
    },
}
TYPE_OF_TOOL = {tool: kind for reqs in REQUESTS.values() for kind, (_, tool) in reqs.items()}


def questions(domain: str) -> dict[str, dict[str, str]]:
    out = {}
    for kind, (what, _) in REQUESTS[domain].items():
        out[f"requested_{kind}"] = {
            "type": "noul",
            "instructions": f"At any point in the CONVERSATION, did the customer ask the agent to {what}?",
        }
        out[f"handled_{kind}"] = {
            "type": "noul",
            "instructions": f"Was every customer request to {what} HANDLED, as defined in the CONVERSATION's notes?",
        }
    return out


def transcript(traj: list[dict[str, Any]]) -> str:
    """The conversation as the customer and agent saw it, with the agent's
    actions (not its lookups) in place."""
    paired = tau.pair_results(traj)
    lines, n = [], 0
    for m in traj:
        role = m.get("role")
        if role == "user":
            text = (m.get("content") or "").replace(tau.STOP, "").strip()
            if text:
                lines.append(f"CUSTOMER: {text}")
        elif role == "assistant":
            text = (m.get("content") or "").strip()
            if text:
                # Agents' long listings (products, flights) matter less than what they say they did.
                lines.append(f"AGENT: {text if len(text) <= MAX_AGENT_CHARS else text[:MAX_AGENT_CHARS] + ' […]'}")
            for tc in m.get("tool_calls") or []:
                fn = tc.get("function") or {}
                name = fn.get("name") or ""
                if audit.is_write(name) or name == "transfer_to_human_agents":
                    args = fn.get("arguments")
                    args = args if isinstance(args, str) else json.dumps(args, ensure_ascii=False)
                    result = str(paired[n][0].get("content") or "") if n in paired else ""
                    lines.append(f"AGENT ACTION: {name}({args}) -> {'error: ' + result[:200] if result.startswith('Error') else 'ok'}")
                n += 1
    return (HANDLED_NOTE + "\n\nCONVERSATION:\n" + "\n\n".join(lines))


HANDLED_NOTE = ("NOTES: A customer request is HANDLED when the agent either carried it out with an AGENT ACTION, or "
                "explicitly declined it and told the customer why. It is not handled if it was left unaddressed, done "
                "only in part, or the agent said it would act but no AGENT ACTION did it. Lookups are not shown.")


def completeness_check(cfg: Config, investigation: Path, baseline: Path, grounded_run: Path | None,
                       budget_usd: float, control: int = 200, dry_run: bool = False) -> dict[str, Any]:
    key = os.environ.get("JEV_API_KEY")
    if not key and not dry_run:
        raise SystemExit("JEV_API_KEY is not set: run under `envchain observer-ai ...`")
    with (investigation / "tau_runs.csv").open() as f:
        runs = {r["trace_id"]: r for r in csv.DictReader(f)}
    with (investigation / "wrong_args.csv").open() as f:
        missed: dict[str, set[str]] = {}
        for g in csv.DictReader(f):
            if g["category"] == "missed" and g["tool"] in TYPE_OF_TOOL:
                missed.setdefault(g["trace_id"], set()).add(TYPE_OF_TOOL[g["tool"]])
    failing = sorted(t for t, r in runs.items() if r["failed"] == "1" and r["missed_write"] == "1")
    ok_pool = sorted(t for t, r in runs.items() if r["failed"] == "0" and r["gold_consistent_task"] == "1")
    ok = sorted(random.Random(cfg.seed).sample(ok_pool, min(control, len(ok_pool))))
    groups = {**{t: "missing_action" for t in failing}, **{t: "control" for t in ok}}

    def answers(folder: Path | None) -> dict[str, dict[str, str]]:
        if folder is None:
            return {}
        with (folder / "runs.csv").open() as f:
            return {r["trace_id"]: r for r in csv.DictReader(f)}

    first, grounded = answers(baseline), answers(grounded_run)
    est = Estimator(cfg.cost.tokenizer)
    work = []
    for run, row in load_tau(cfg):
        if run.trace_id in groups:
            domain = run.meta["domain"]
            work.append((run, domain, transcript(row["traj"]), questions(domain)))
    # A seeded order, so a run stopped by the budget cuts evenly across groups.
    random.Random(cfg.seed + 1).shuffle(work)
    estimate_all = sum(est.count(st) + est.count(json.dumps(q)) for _, _, st, q in work)
    if dry_run:
        return {"dry_run": True, "runs": len(work), "missing_action_runs": len(failing), "control_runs": len(ok),
                "estimated_tokens": estimate_all, "projected_usd_at_jev_rate": cfg.cost.usd(int(estimate_all * 1.33))}

    guard = BudgetGuard(budget_usd, cfg.cost.usd(1))
    rows, requests = [], []
    aborted = ""
    with httpx.Client(timeout=120, headers={"Authorization": f"Bearer {key}"}) as client:
        for run, domain, state, qs in work:
            estimate = est.count(state) + est.count(json.dumps(qs))
            try:
                held = guard.reserve(estimate)
            except BudgetExceeded as e:
                aborted = str(e)
                break
            billed = None
            out: dict[str, Any] = {}
            try:
                resp = client.post(ENDPOINT, json={"model": cfg.tailwise.model, "state": state, "questions": qs})
                if resp.status_code == 200:
                    out = resp.json()
                    billed = int((out.get("usage") or {}).get("input_tokens") or estimate)
                else:
                    out = {"error": f"HTTP {resp.status_code}: {resp.text[:200]}"}
            except httpx.TransportError as e:
                out = {"error": f"{type(e).__name__}: {e}"}
            finally:
                guard.settle(held, estimate, billed)
            a = out.get("answers") or {}
            def noul(q: str) -> float | None:
                v = (a.get(q) or {}).get("noul")
                return float(v) if v is not None else None

            req = {k: noul(f"requested_{k}") for k in REQUESTS[domain]}
            hand = {k: noul(f"handled_{k}") for k in REQUESTS[domain]}
            both = {k: (r_, h_) for k in REQUESTS[domain] if (r_ := req[k]) is not None and (h_ := hand[k]) is not None}
            unhandled = sorted(k for k, (r_, h_) in both.items() if r_ >= 0.5 and h_ < 0.5)
            score = max((min(r_, 1 - h_) for r_, h_ in both.values()), default=None)
            gold_missed = sorted(missed.get(run.trace_id, set()))
            r = runs[run.trace_id]
            rows.append({
                "group": groups[run.trace_id], "run_id": run.run_id, "trace_id": run.trace_id, "domain": domain,
                "failed": int(r["failed"]), "gold_consistent_task": r["gold_consistent_task"],
                "requested": "; ".join(k for k in REQUESTS[domain] if (req[k] or 0) >= 0.5),
                "unhandled": "; ".join(unhandled), "gold_missed": "; ".join(gold_missed),
                "flag": int(bool(unhandled)), "score": score,
                "right_reason": int(bool(set(unhandled) & set(gold_missed))),
                "answer_wrong_first": first.get(run.trace_id, {}).get("answer_answer_wrong", ""),
                "answer_wrong_grounded": grounded.get(run.trace_id, {}).get("answer_answer_wrong", ""),
                **{f"p_requested_{k}": req[k] for k in REQUESTS[domain]},
                **{f"p_handled_{k}": hand[k] for k in REQUESTS[domain]},
                "tokens": billed, "model": out.get("model", ""), "error": out.get("error", ""),
            })
            requests.append({"run_id": run.run_id, "request": {"state": state, "questions": qs}, "response": out})
    write_csv(investigation / "completeness_check.csv", rows)
    write_json(investigation / "completeness_check_requests.json", requests)
    result = {"runs": len(rows), "planned_runs": len(work), "spent_usd": guard.spent_usd, "tokens": guard.spend.tokens,
              "aborted": aborted, "errors": sum(1 for r in rows if r["error"]),
              "jev_models": sorted({r["model"] for r in rows if r["model"]})}
    write_json(investigation / "completeness_check_summary.json", result)
    return result
