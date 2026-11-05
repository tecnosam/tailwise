# Changelog

All notable changes to Tailwise are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- `scorer.evidence` defaults to `grounded`: the model sees each tool call's
  result on its line, preferring the results later calls rely on. With
  `first`, it saw only the first three results, so on τ-bench it flagged what
  it could not check. With `grounded`, τ-bench ROC-AUC rises from 0.59 to 0.64
  (0.63 to 0.71 on the runs whose label the trace can show), false alarms on
  successful runs fall from 57% to 33%, and SWE-agent ROC-AUC rises from 0.79
  to 0.80 on a held-out sample. It costs about twice the input tokens. Set
  `evidence: first` for the old behaviour.

- `severity` is the deciding questions' average answer, on every scored trace,
  instead of the priority-weighted sum over the flags only. It is the order to
  review traces in: a trace several checks doubt ranks above one a single
  check is sure of. On τ-bench with the action checks (trace-detectable
  runs), when 10% of runs fail and a team reviews the top 5%, the share of
  reviewed traces that are failures rises from 23% (ranked by `p_keep`) to
  47% (95% interval 26–66%). The benchmark's headline metric is now that share. The response's `severity` is `null` on
  fallback rather than omitted. Rank-order priority weights are gone. A check
  that failed is left out of the average rather than counted as 0, so a
  scorer outage does not bury failed traces; `action_wrong` on a trace without
  actions is 0.

### Added

- Heuristic rules: free, certain checks that need no model, in the config
  (every agent) and in a manifest (one agent; same-name rules override,
  `enabled: false` removes). They check the final answer, tool outputs and
  arguments (length, regular expressions, JSON, JSON Schema, `ok_status`),
  and the run (`max_tool_calls`, `max_repeated_calls`, `requires_tools`,
  `forbids_tools`). `on_fail: keep` keeps the trace without asking the
  model (rule `keep_rule`, severity 1); `signal` counts 1 or 0 in severity.
  Fired rules are in the response's `rules`, the log and `eval.rules`.

- Action checks, turned on by a manifest `actions` block naming the tools that
  act for the user. `action_wrong` checks each action call's arguments against
  the user's request and the lookups they came from; `unhandled_request`
  checks that every request of those kinds was carried out or declined. Both
  can keep a trace, with a reason naming the action or the request. On
  τ-bench they raise the failures caught from 54% to 70% (wrong-variant
  exchanges from 33% to 90%), keeping 40% of successful runs instead of 26%.
- Traces carry the conversation, from the latest LLM span's messages or the
  JSON API's `conversation`, for the completeness check.
- Code-writing agents, such as a smolagents `CodeAgent`, are read: each step
  under the `AGENT` root whose model wrote code becomes an `execute_code`
  tool call, with the code as its arguments, what it printed as its output,
  and the step's error as its status. Such agents record no TOOL spans for
  their actions, so the model saw only the question and the answer. On
  TRAIL's SWE-bench runs, ROC-AUC rose from 0.33 to 0.67.

### Fixed

- An `AGENT` root span with no `output.value` (the run crashed or ran out of
  steps) is scored with an empty final answer instead of being discarded as
  not scorable. Those runs are failures, and they were dropped unreviewed.
- A trace too long for the model is no longer dropped at random. The middle of
  its longest fields (a giant patch, a long log) is cut until the state fits
  in 40,000 characters, and the decision says so: `trimmed` in the response
  and decision log, `eval.trimmed` on the exported trace.
- A root span whose input is `{"task": ...}`, as smolagents records it, gives
  the task as the question instead of the raw JSON.
- A tool call whose output starts with `Error:` is read as failed, even when
  its span status is OK, as many tools report failures that way.

## [0.1.0] - 2026-10-27

The first release of the Tailwise inference engine.

### Added

- Decides which LLM agent traces are worth keeping for review: each trace is
  scored by a System One decision model, TypeSafe's hosted Jev by default, and
  a keep/drop policy (threshold and random floor) decides.
- A plain-English reason with every decision, saying why the model thinks the
  final answer is wrong, with a short `cause` and the policy `rule`.
- Intake over OpenTelemetry (OTLP gRPC and HTTP), with spans assembled into a
  trace once it goes quiet, and over a JSON API, queued or scored synchronously.
- NATS JetStream queues with redelivery, backoff and dead-letter streams, on an
  external server or one embedded in the engine.
- Export of kept traces to any number of OTLP/HTTP back ends (Phoenix,
  Langfuse, an OpenTelemetry Collector), each from its own durable queue.
- Runs with no config file: the built-in defaults need only `JEV_API_KEY`.
  Manifests set the questions a model is asked and are optional.
- Five built-in checks asked alongside `answer_wrong` in the same call:
  `unsupported_answer`, `missing_tool_calls`, `answered_on_failure`,
  `ignored_instructions` and `contradicts_knowledge`. Their answers are
  recorded with every decision; `answer_wrong` alone decides keep or drop.
- A manifest `decision` block (none in the built-in questions): `any_of`
  lists more questions that can keep a trace, in priority order after the
  decision question, and `thresholds` gives any of them its own threshold.
- Flags and severity on every decision: the questions that reached their
  threshold, and a priority-weighted score that ranks kept traces for review.
  Both appear in the decision log, with every answer, and on exported traces
  as `eval.flags`, `eval.severity` and `eval.answers.<question>`.
- `scorer.system_prompt` (off by default) shows the model the agent's system
  prompt, above the question. It comes from the JSON API's `system_prompt`,
  the root span's `tailwise.system_prompt`, or the first LLM span's system
  message, and is cut to 8,000 characters. Off, or without a prompt, the
  model sees exactly what it did before.
- `scorer.evidence: grounded` (experimental; the default, `first`, is
  unchanged) shows each tool call's result on its line, within
  `scorer.evidence_budget` characters, preferring the results later calls
  rely on. The default shows only the first three tool results, so on long
  runs the model never sees most of what the agent looked up. Tool calls carry
  their `output` (JSON API and OTLP).
- `POST /v1/otlp/traces:score`: scores one trace sent as OTLP spans,
  synchronously, built exactly as the OTLP receivers build it.
- Score responses include every answer, the flags, the severity and the
  input tokens Jev reported billing.
- A benchmark harness in `bench/` that measures how well Tailwise picks
  failed agent runs compared with random sampling, on τ-bench and SWE-agent
  runs (and TRAIL).
- One model per agent, with its own questions, endpoint or key.
- The engine's own metrics and errors over OpenTelemetry.
- Docker Compose for a local stack, a Helm chart for Kubernetes, and
  `tailwise -check` to validate a configuration before a rollout.
