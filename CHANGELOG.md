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

### Fixed

- A trace too long for the model is no longer dropped at random. The middle of
  its longest fields (a giant patch, a long log) is cut until the state fits
  in 40,000 characters, and the decision says so: `trimmed` in the response
  and decision log, `eval.trimmed` on the exported trace.
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
