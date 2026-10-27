# Changelog

All notable changes to Tailwise are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
- One model per agent, with its own questions, endpoint or key.
- The engine's own metrics and errors over OpenTelemetry.
- Docker Compose for a local stack, a Helm chart for Kubernetes, and
  `tailwise -check` to validate a configuration before a rollout.
