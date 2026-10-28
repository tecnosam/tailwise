# Tailwise inference engine

Decides which LLM agent traces are worth keeping for review. For each trace it
asks a System One decision model, TypeSafe's hosted Jev by default, whether
the final answer is likely wrong, applies a keep/drop policy, and exports the
kept traces over OTLP to any number of trace back ends:
[Arize Phoenix](https://github.com/Arize-ai/phoenix), [Langfuse](https://langfuse.com),
an OpenTelemetry Collector, or anything else that accepts OTLP/HTTP traces.

Traces arrive as OpenTelemetry spans (OTLP over gRPC or HTTP) or over an HTTP
JSON API, and wait in a NATS JetStream queue: on the server `queue.url`
names, or, when it is empty, on a NATS server the engine runs inside its own
process. A worker pool in the same binary pulls them off the queue and does
the rest.

```
OTLP gRPC :4317 ─┐
OTLP HTTP :4318 ─┴─> spans stored by trace ID ──> assembled once the trace goes quiet ─┐
                     (stream TAILWISE_SPANS)                                            │
                                                                                        v
POST /v1/traces ──> validate ───────────────────────────────> JetStream stream TAILWISE_TRACES
                                                                                        │
                                                                                        v
                                             worker pool ──> slice ──> Jev ──> policy ──> exporters (kept only)
                                                  │
                               system error, after max_deliver attempts
                                                  v
                                    JetStream stream TAILWISE_TRACES_DLQ

POST /v1/traces:score ──> validate ──> slice ──> Jev ──> policy ──> exporters   (no queue)
```

## Quickstart

You need Docker and a Jev API key. The engine needs no config file.

```sh
export JEV_API_KEY=...        # your TypeSafe Jev API key
make up                       # the engine, NATS JetStream and Phoenix, in Docker
make samples                  # send the bundled sample traces and print each decision
```

`make samples` sends [`scripts/samples.jsonl`](scripts/samples.jsonl), a
dozen agent runs whose answers are wrong, partly correct or correct, and
prints each decision next to its label, with Jev's reason in plain English:

```
a57d9106a210b9f56fba917038ea3be2  label=wrong    keep p_keep=0.980  above_threshold
    The final answer contradicts the evidence the agent gathered.
d5ca6bb1e89aa96b4290758d031c7716  label=correct  drop p_keep=0.040  below_threshold
    The final answer is correct, complete and supported by the evidence.
```

The kept ones appear in Phoenix at http://localhost:6006, in the project
`tailwise`, with the decision as `eval.*` attributes on their root span.
`make logs` follows the engine's log; `make down` stops everything.

At start-up the engine warns that the model has no manifest: it asks the
built-in questions, which are written for any agent rather than yours. See
[Manifests](#manifests).

## Setup

Requirements:

- A Jev API key, in `JEV_API_KEY`
- Docker, to run the engine, NATS and Phoenix with Compose
- Go 1.26 or newer, to build or test outside Docker
- Python 3, only for `make samples`

### With Docker Compose

```sh
make up       # docker compose up --build -d: the engine, NATS JetStream and Phoenix
make logs     # follow the engine's log
make down     # stop all three; queued traces, dead letters and Phoenix's data stay in volumes
```

Compose starts three services:

| Service | Host ports | Notes |
| --- | --- | --- |
| `observer` (container `tailwise-observer`) | 8080 (JSON API), 4318 (OTLP HTTP), **14317** (OTLP gRPC) | The engine: receivers, API and worker pool, on its built-in defaults |
| `nats` | 4222 (clients), 8222 (monitoring), both on loopback only | JetStream, stored in the `nats-data` volume; configured by `nats.conf` |
| `phoenix` | 6006 (UI and OTLP HTTP), 4317 (OTLP gRPC) | Phoenix, stored in the `phoenix-data` volume. Open http://localhost:6006 |

Inside the container the engine listens for OTLP gRPC on 4317. Compose
publishes it on host port 14317 because Phoenix takes 4317 for its own OTLP
gRPC receiver.

If a Phoenix is already running on the host (`phoenix serve`), it holds 6006
and 4317 and `make up` fails with "port is already allocated". Either stop
it, or move the compose one with `PHOENIX_PORT` and `PHOENIX_GRPC_PORT`, or
keep using the host one:

```sh
TAILWISE_EXPORT_ENDPOINT=http://host.docker.internal:6006/v1/traces \
  docker compose up --build -d --no-deps nats observer
```

Compose reads these variables:

| Variable | Default |
| --- | --- |
| `JEV_API_KEY` | (required) your Jev API key |
| `TAILWISE_EXPORT_ENDPOINT` | `http://phoenix:6006/v1/traces` (the first exporter) |
| `PHOENIX_PORT` | `6006` |
| `PHOENIX_GRPC_PORT` | `4317` |
| `TAILWISE_PORT` | `8080` |
| `TAILWISE_OTLP_GRPC_PORT` | `14317` |
| `TAILWISE_OTLP_HTTP_PORT` | `4318` |

To run the engine with your own config instead, mount it and pass `-config`;
[`examples/README.md`](examples/README.md#docker-compose) shows how.

### On the host

```sh
make phoenix  # start only Phoenix in Docker
make run      # build and start the engine on its built-in defaults (Jev, embedded NATS)
make run CONFIG=examples/jev.yaml   # or with a config file
make test     # go vet + go test -race, fully offline

# Or with the queues in an external NATS JetStream, as in a cluster:
make nats
TAILWISE_NATS_URL=nats://localhost:4222 make run
```

`make run` binds 8080, 4317 and 4318. Phoenix, from `make phoenix` or on the
host, already holds 4317, and the engine exits with an "address already in
use" error; start it with `TAILWISE_OTLP_GRPC_ADDR=:14317 make run` instead.

### Sending traces

```sh
curl -X POST localhost:8080/v1/traces:score -d @testdata/traces/wrong_river.json
```

```json
{"trace_id":"wrong-river-001","decision":"keep","p_keep":0.98,"rule":"above_threshold","reason":"The final answer contradicts the evidence the agent gathered.","cause":"contradicts_evidence","model":"jev-1.14.0","latency_ms":357.8,"exported":true}
```

[`scripts/send_samples.py`](scripts/send_samples.py) sends a batch:

```sh
python3 scripts/send_samples.py --sync          # print each decision next to its label
python3 scripts/send_samples.py                 # through the queue
python3 scripts/send_samples.py --otlp          # as OTLP spans, to :4318
python3 scripts/send_samples.py --dataset my-traces.jsonl -n 50   # your own traces
```

`--dataset` takes any JSONL file of traces in the JSON API's shape; an
optional `label` on each row is printed next to its decision, and `-n`
samples that many, equally across labels. With `--otlp` each trace is sent
as an AGENT root span and one TOOL span per tool call.

To point an instrumented agent at the engine, give its OTLP exporter the
engine's endpoint instead of Phoenix's; see
[Connecting an agent or service](#connecting-an-agent-or-service).

Flags: `-config PATH` (optional: without it the engine runs on its built-in
defaults and the environment), `-check` (validate and exit), `-version` and
`-log-level debug|info|warn|error`.

## Configuration

The engine needs no config file. Without `-config` it runs on its built-in
defaults: Jev as the scorer, the built-in questions, an embedded NATS
server and one Phoenix exporter at `http://localhost:6006/v1/traces`. A
config file sets only what differs; every key it leaves out keeps its
built-in default. [`examples/`](examples/) has ready-to-run variations, and
[tailwise.schema.yaml](tailwise.schema.yaml) is the full reference: a JSON
Schema with each key's type, default, limits and the rules that tie keys
together. [architecture.md](architecture/architecture.md) explains how a file is loaded.

| Key | Default | Meaning |
| --- | --- | --- |
| `server.addr` | `:8080` | Listen address |
| `server.workers` | `8` | Workers pulling traces from the queue, so concurrent calls to the model |
| `server.shutdown_timeout` | `30s` | Time for in-flight traces to finish on shutdown |
| `otlp.grpc_addr` | `:4317` | OTLP gRPC listen address; `""` turns gRPC off |
| `otlp.http_addr` | `:4318` | OTLP HTTP listen address (`POST /v1/traces`); `""` turns it off |
| `otlp.idle_timeout` | `10s` | A trace with no new spans for this long is assembled and scored |
| `otlp.max_wait` | `2m` | A trace is assembled this long after its first span even if spans keep arriving |
| `otlp.assemblers` | `4` | Goroutines assembling traces |
| `otlp.span_max_age` | `1h` | Spans never assembled are removed after this |
| `queue.url` | empty | NATS server; empty runs one embedded in the engine. See [Where the queues live](#where-the-queues-live) |
| `queue.store_dir` | empty | Embedded NATS only: where its streams are kept; empty is a temporary directory, lost on restart |
| `queue.connect_timeout` | `30s` | How long to wait for NATS at start-up before exiting |
| `queue.prefix` | `tailwise` | Names every NATS stream, subject and consumer; engines with the same prefix share the work. See [NATS names](#nats-names) |
| `queue.max_messages` | `100000` | Queued traces before `POST /v1/traces` returns 503 |
| `queue.ack_wait` | `2m` | Time a worker may hold a trace before it is redelivered |
| `queue.max_deliver` | `5` | Delivery attempts before a trace is dead-lettered |
| `queue.redelivery_backoff` | `[5s, 30s, 2m, 5m]` | Delay before each redelivery; the last value repeats |
| `queue.dlq_max_age` | `168h` | How long dead letters (traces and exports) are kept |
| `queue.replicas` | `1` | Copies of each stream a NATS cluster keeps; 3 on a three-server cluster |
| `queue.auth` | none | NATS authentication: one of `credentials_file`, `nkey_seed_file`, `user` with `password_env`, or `token_env` |
| `queue.tls` | none | NATS TLS: `ca_file` to verify the server, `cert_file` and `key_file` for mutual TLS |
| `scorer.kind` | `jev` | Which System One model: `jev` (TypeSafe's hosted Jev), or another such as `kev` or `laya` |
| `scorer.api_key_env` | `JEV_API_KEY` for `jev`, unset otherwise | Environment variable holding the model's API key, sent as a bearer token. Start-up fails if it names an unset variable |
| `scorer.endpoint` | `https://api.typesafe.ai/v1/systemone` | The model's `/v1/systemone` URL |
| `scorer.model` | `jev-latest` | Model name sent with every request |
| `scorer.manifest` | none | Optional manifest: the questions the model is asked. See [Manifests](#manifests) |
| `scorer.decision_question` | `answer_wrong` | Question that drives the decision; must be `noul` |
| `scorer.reason_question` | `why_wrong` | Choice question whose chosen option is the plain-English `reason`; see [Reasons](#reasons). Empty turns it off |
| `scorer.timeout` | `30s` | Timeout per call |
| `scorer.max_retries` | `3` | Retries after the first attempt, on the synchronous endpoint only; a queued trace is redelivered by the queue instead |
| `scorer.backoff` | `[1s, 5s, 15s]` | Base delay before each synchronous retry; the last value repeats |
| `agents.attributes` | `[agent.name, service.name]` | Span attributes naming a trace's agent, tried on the root span then its resource. See [One model per agent](#one-model-per-agent) |
| `agents.models` | none | Agent name to its own model: `scorer` keys, with `endpoint` and `model` required; nothing comes from `scorer` |
| `policy.keep_threshold` | `0.5` | Keep when `p_keep` reaches this |
| `policy.random_floor` | `0.02` | Share of below-threshold traces kept at random |
| `observability.error_sinks` | `[{kind: stdout}]` | Where the engine's own errors are written as structured log records: `stdout` and `file` (with a `path`) as JSON lines, `otlp` (with an `endpoint`, `headers`, `headers_env`) as OpenTelemetry logs |
| `observability.metrics.endpoint` | empty (off) | OTLP/HTTP metrics URL for the engine's own metrics. See [Metrics](#metrics) |
| `observability.metrics.headers`, `headers_env` | none | Headers for the metrics endpoint, as for an exporter |
| `observability.metrics.interval` | `15s` | How often metrics are sent |
| `exporters` | one Phoenix at `http://localhost:6006/v1/traces` | The trace back ends kept traces go to; see [Exporters](#exporters). An empty list exports nothing |
| `exporters[].name` | (required) | Names the back end in logs, errors and its NATS stream: letters, digits, `-`, `_`; unique |
| `exporters[].endpoint` | (required) | OTLP/HTTP traces URL, including the path |
| `exporters[].headers` | none | Headers sent as written. Keep secrets out of the file |
| `exporters[].headers_env` | none | Environment variable holding more headers, as `key=value,key2=value2`. Start-up fails if it is unset |
| `exporters[].resource_attributes` | none | Resource attributes added to every span sent to this back end |
| `exporters[].queue_size` | `1024` | Kept traces waiting for this back end; past it a trace is skipped for it and the skip reported |
| `exporters[].concurrency` | `4` | Traces sent to this back end at once |
| `exporters[].max_deliver` | `3` | Tries before the exporter gives up on a trace |
| `exporters[].backoff` | `[1s, 5s, 30s]` | Wait before each retry; the last value repeats |

Environment overrides, which apply with or without a config file:

| Variable | Overrides |
| --- | --- |
| `TAILWISE_NATS_URL` | `queue.url` |
| `TAILWISE_SCORER_ENDPOINT` | `scorer.endpoint` |
| `TAILWISE_EXPORT_ENDPOINT` | `exporters[0].endpoint` |
| `TAILWISE_MANIFEST` | `scorer.manifest` |
| `TAILWISE_OTLP_GRPC_ADDR` | `otlp.grpc_addr` |
| `TAILWISE_OTLP_HTTP_ADDR` | `otlp.http_addr` |

The standard `OTEL_EXPORTER_OTLP_HEADERS` and `OTEL_EXPORTER_OTLP_TRACES_HEADERS`
are not read: each exporter sends only its own headers, so one back end's
credentials never reach another. Use `headers_env` instead.

The engine validates its configuration at start-up and exits with a clear
error if:

- `JEV_API_KEY`, or a variable the config names for a secret, is not set
- a threshold is outside [0, 1], a duration does not parse, or a key is unknown
- the decision question is missing from the manifest (or the built-in
  questions) or is not `noul`
- `queue.ack_wait` is not longer than the slowest model's `timeout` (a
  queued trace gets one scoring call per delivery), since a shorter value
  would hand a trace to a second worker while the first is still on it

`tailwise -check` runs the same checks and exits, printing any warnings.

### Manifests

A manifest holds the questions the model is asked:

```json
{"model": "...", "questions": {"answer_wrong": {"type": "noul", "instructions": "..."}, "why_wrong": {"type": "choice", "criteria": {"...": "..."}}}}
```

- `questions` is sent to the model unchanged, all questions, in the
  manifest's order, in one call. The reason question's chosen option is the
  `reason`.
- `decision`, optional, says how the yes/no (`noul`) answers make `p_keep`
  (see [Combining the answers](#combining-the-answers)). Without it, `p_keep`
  is the decision question's answer. It stays in the engine and is not sent
  to the model.
- A relative `scorer.manifest` path resolves against the config file's
  directory.

A manifest is optional, for every System One model. Without one the model
is asked the built-in questions
([`builtin_manifest.json`](internal/config/builtin_manifest.json)), and the
engine logs a warning at start-up. They are:

| Question | Weight | Asks |
| --- | --- | --- |
| `answer_wrong` | 0.5 | Is the final answer wrong or only partially correct? |
| `unsupported_answer` | 0.1 | Does it make claims the evidence does not support? |
| `missing_tool_calls` | 0.1 | Did the agent skip lookups the task needed? |
| `answered_on_failure` | 0.1 | Did it answer despite a failed or partial tool call? |
| `ignored_instructions` | 0.1 | Does it go against the user's instructions? |
| `contradicts_knowledge` | 0.1 | Does it contradict general knowledge? |
| `why_wrong` | | Which [reason](#reasons) explains what is wrong? |

Each yes/no answer is the probability that the agent failed in that way, and
the weights combine them into `p_keep`. To word the questions
for your agent, start from
[`examples/manifests/my-agent.json`](examples/manifests/my-agent.json) and
set `scorer.manifest` (or `TAILWISE_MANIFEST`).

### Combining the answers

A manifest's `decision` block weights its yes/no questions:

```json
"decision": {
  "intercept": 0,
  "weights": {"answer_wrong": 0.5, "unsupported_answer": 0.1, "missing_tool_calls": 0.1,
              "answered_on_failure": 0.1, "ignored_instructions": 0.1, "contradicts_knowledge": 0.1}
}
```

and `p_keep` is a weighted [logarithmic opinion pool](https://en.wikipedia.org/wiki/Logarithmic_opinion_pool)
of their answers:

```
p_keep = sigmoid(intercept + Σ weight × logit(answer))      logit(p) = ln(p / (1 − p))
```

- **Weights summing to 1, no intercept** (as in the built-in questions):
  `p_keep` stays on the answers' own scale. Answers that agree keep their
  probability (all at 0.9 give 0.9); answers that disagree meet in between.
  A larger weight gives a question more say.
- **Fitted weights.** The formula is a logistic regression on the answers'
  logits, so the coefficients and intercept of one fitted on labelled traces
  can be used as they are. They need not sum to 1.
- **Rules.** Every weighted question must be a `noul` question of the same
  manifest, and the model must answer all of them: a missing answer is a
  scoring error, like a missing decision answer. Questions without a weight
  are still asked and logged, but do not change `p_keep`.
- **Changing the weights changes `p_keep`,** so recheck `policy.keep_threshold`
  against the share of traces kept.

### One model per agent

Each agent can have its own model: its own questions, and
if needed its own endpoint or key. The `scorer` section is the default
model; `agents.models` adds one per agent, each complete on its own:

```yaml
agents:
  attributes: [agent.name, service.name]
  models:
    support-bot:
      endpoint: https://api.typesafe.ai/v1/systemone
      model: jev-latest
      manifest: manifests/support-bot.json
    research-agent:
      endpoint: https://api.typesafe.ai/v1/systemone
      model: jev-latest
      manifest: manifests/research-agent.json
      timeout: 45s
```

- **Which agent.** For OTLP traces, the first of `agents.attributes` set on
  the root span, else on its resource: OpenInference's `agent.name`, then the
  resource's `service.name` by default. For the JSON API, the trace's
  `agent` field.
- **Which model.** The agent's entry under `agents.models`, else the
  `scorer` section. A trace that names no agent, or an agent not listed, is
  scored by the default.
- **No inheritance.** Each entry takes the `scorer` keys (`kind`,
  `endpoint`, `model`, `api_key_env`, `manifest`, `decision_question`,
  `timeout`, `max_retries`, `backoff`) and is complete on its own:
  `endpoint` and `model` are required, and nothing is taken from the
  `scorer` section. Keys it leaves out get the engine's built-in defaults,
  the same ones `scorer` gets; without a manifest it gets the built-in
  questions and its own warning. `TAILWISE_SCORER_ENDPOINT` and
  `TAILWISE_MANIFEST` change only the `scorer` section.
- **Everywhere.** The decision log, the `/v1/traces:score` response, the
  `eval.agent` span attribute and the metrics all carry the agent, and
  `eval.model` the model. `/readyz` probes the default model only, so one
  agent's model being down does not make the whole engine unready.

[`examples/agents.yaml`](examples/agents.yaml) is a complete example.

## OTLP ingestion

The engine runs the standard OTLP trace receivers, so an agent's exporter can
point at it as if it were a collector:

| Protocol | Address | Details |
| --- | --- | --- |
| gRPC | `:4317` | `opentelemetry.proto.collector.trace.v1.TraceService/Export`, gzip accepted |
| HTTP | `:4318`, `POST /v1/traces` | `application/x-protobuf` or `application/json`, gzip accepted, 16 MiB limit |

An export is answered with success as soon as the spans are stored (in NATS,
once it has confirmed them, typically within a few milliseconds). If they
cannot be stored the answer is gRPC `UNAVAILABLE` or HTTP 503, which OTLP
exporters retry.

### Connecting an agent or service

Point the OTLP exporter at the engine instead of at Phoenix. Nothing else in
the agent changes, and no API key or header is needed.

For anything using an OpenTelemetry SDK, the standard variables are enough:

```sh
# HTTP (the SDK appends /v1/traces)
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf

# or gRPC (host port 14317 under Docker Compose, 4317 inside the network)
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:14317
export OTEL_EXPORTER_OTLP_PROTOCOL=grpc
```

Or set it in code. In Python, with an OpenInference instrumentor:

```python
from opentelemetry import trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor

provider = TracerProvider()
provider.add_span_processor(
    BatchSpanProcessor(OTLPSpanExporter(endpoint="http://localhost:4318/v1/traces"))
)
trace.set_tracer_provider(provider)
# then instrument as usual, e.g. GoogleADKInstrumentor().instrument(tracer_provider=provider)
```

The engine needs one root span carrying the question in `input.value` and the
answer in `output.value`; tool calls are spans with
`openinference.span.kind` = `TOOL`. The instrumentors emit these already. To
send a trace by hand:

```sh
curl -i http://localhost:4318/v1/traces -H 'Content-Type: application/json' -d '{
  "resourceSpans": [{"scopeSpans": [{"spans": [{
    "traceId": "5b8efff798038103d269b633813fc60c",
    "spanId": "eee19b7ec3c1b174",
    "name": "agent",
    "startTimeUnixNano": "1760000000000000000",
    "endTimeUnixNano": "1760000001000000000",
    "attributes": [
      {"key": "openinference.span.kind", "value": {"stringValue": "AGENT"}},
      {"key": "input.value", "value": {"stringValue": "Which river flows through the capital of France?"}},
      {"key": "output.value", "value": {"stringValue": "The Rhône"}}
    ]
  }]}]}]
}'
```

It answers `200 {}`. The decision appears in the engine's log about
`otlp.idle_timeout` (10s) later, and the trace shows up in your exporters
(Phoenix by default) if it was kept.

### From spans to a decision

1. **Store.** Each export is split by trace ID and published to
   `tailwise.spans.<trace id>` in the `TAILWISE_SPANS` stream.
2. **Wait.** A trace's spans usually arrive in several batches, with the root
   span last. The trace is considered complete when no span has arrived for
   `otlp.idle_timeout`, or `otlp.max_wait` after its first span.
3. **Assemble.** The spans are merged (a span sent twice counts once) and
   turned into the same record the JSON API takes; see the mapping below.
   Spans that do not describe an agent run are discarded here, before they
   cost a call to the model.
4. **Queue, score, decide.** The record goes to `TAILWISE_TRACES` and from
   there everything is as for a JSON trace, including redelivery and the
   dead-letter stream.
5. **Forward.** A kept trace is sent to every exporter as its original spans,
   with the original trace and span IDs, the exporter's
   `resource_attributes`, and the `eval.*` attributes added to the root
   span. A dropped trace is sent nowhere and its spans are deleted.

Because of step 2, a decision arrives at least `otlp.idle_timeout` after the
trace's last span.

### How spans are read

The engine reads the [OpenInference](https://github.com/Arize-ai/openinference)
attributes that Phoenix's instrumentors emit.

| Trace field | Taken from |
| --- | --- |
| Root span | The span whose parent is not in the trace, preferring `openinference.span.kind` = `AGENT`, then `CHAIN` |
| `agent` | The first of `agents.attributes` set on the root span, else on its resource |
| `question` | Root span `input.value` |
| `final_answer` | Root span `output.value` |
| `tool_calls` | Spans with kind `TOOL`, ordered by start time |
| tool `name` | `tool.name`, else the span name |
| tool `args` | `input.value` if it is JSON, else `tool.parameters`, else `input.value` as a string |
| tool `status` | `tailwise.tool.status` if set; otherwise `error` when the span status is ERROR, else `ok` |
| `evidence` | First of: `tailwise.evidence.<i>` on the root span; documents on `RETRIEVER` spans; the paragraphs (split on blank lines) of successful tool outputs |

When `input.value` or `output.value` holds a JSON object, the text is taken
from a `question`/`input`/`query`/`prompt` key (or `answer`/`output`/`result`
for outputs) when there is one. A tool output shaped as a function response,
`{"response": {"result": "..."}}`, is unwrapped.

A message recorded by the Google ADK instrumentor,
`{"new_message": {"parts": [{"text": "..."}]}}` on the way in and
`{"content": {"parts": [{"text": "..."}]}}` on the way out, gives the text of
its parts, so an ADK agent's own root span works without extra attributes.

A trace is **not scorable**, and is discarded with a `spans discarded` log
line, when its root span has no `input.value` or no `output.value`. That
covers ordinary service spans sent to the engine by mistake.

An agent run that ended without an answer is scored like any other, as long
as its root span sets `output.value`, even to an empty string. The model then
sees an empty final answer.

### Matching what the model was trained on

The model was trained on evidence as bare paragraph text and on the agent's
own tool statuses (`ok`, `not_found`, `bad_args`, ...). Standard spans carry
neither exactly: a tool span holds the tool's rendered output, and its status
is only OK or ERROR. So for a trace read from plain instrumentor spans, the
slice differs a little from the training data. For the test agent, evidence
items keep their `[Title] ` prefix and a search that found nothing shows as
`ok` with its message as evidence.

An agent can remove the difference by setting two attributes itself:

| Attribute | On | Value |
| --- | --- | --- |
| `tailwise.evidence.0`, `tailwise.evidence.1`, ... | the root span | the evidence items, in order |
| `tailwise.tool.status` | each `TOOL` span | the tool call's status |

With both set, a trace sent as spans yields byte for byte the slice the same
trace yields through the JSON API. A test checks this for every fixture, and
`scripts/send_samples.py --otlp` sends spans this way.

## API

### `POST /v1/traces`

Stores a trace in the queue. The 202 is sent only after JetStream has
confirmed the trace, so an accepted trace survives a restart of the engine,
unless the embedded server runs without `queue.store_dir`.

```json
{
  "trace_id": "7f3a91c2e8",
  "agent": "support-bot",
  "question": "Which river flows through the capital of France?",
  "tool_calls": [{"name": "search", "args": {"query": "capital of France"}, "status": "ok"}],
  "evidence": ["Paris is the capital of France.", "The Seine flows through Paris."],
  "final_answer": "The Seine"
}
```

- `question` and `final_answer` are required. `final_answer` may be an empty
  string for a run that ended without an answer.
- `trace_id` is optional; the engine generates 32 hex characters if it is missing.
- `agent` is optional and picks the model (see [One model per agent](#one-model-per-agent)); at most 128 bytes.
- `args` may be any JSON value. `status` defaults to `ok`.
- This is the engine's own JSON API on port 8080. The OTLP receiver also
  serves a `/v1/traces` path, on port 4318, with a different body.

| Status | Body | When |
| --- | --- | --- |
| 202 | `{"trace_id": "..."}` | Stored in the queue |
| 400 | `{"error": "..."}` | Invalid JSON or a missing required field |
| 413 | `{"error": "..."}` | Body larger than 8 MiB |
| 503 + `Retry-After` | `{"error": "..."}` | Queue at `queue.max_messages`, or NATS unreachable |

### `POST /v1/traces:score`

Same input, scored synchronously without the trace queue. Meant for testing
and demos. It returns as soon as the decision is made; a kept trace is
queued for export and reaches every exporter's back end moments later, so a
slow back end never holds up the response.

```json
{
  "trace_id": "wrong-river-001",
  "agent": "",
  "decision": "keep",
  "p_keep": 0.98,
  "rule": "above_threshold",
  "reason": "The final answer contradicts the evidence the agent gathered.",
  "cause": "contradicts_evidence",
  "model": "jev-1.14.0",
  "latency_ms": 357.8,
  "exported": true
}
```

| Field | Values |
| --- | --- |
| `decision` | `keep` or `drop` |
| `p_keep` | The model's probability that the final answer is wrong (its `noul` for the decision question); `null` on fallback |
| `rule` | Which part of the policy decided: `above_threshold`, `random_floor`, `below_threshold` or `fallback` |
| `reason` | Why the model thinks the final answer is wrong, or that it is right, in plain English (see [Reasons](#reasons)) |
| `cause` | The model's short name for that reason, e.g. `contradicts_evidence` or `correct`; omitted when it gave none |
| `latency_ms` | The model's reported latency; the measured time if it reports none, or on fallback |
| `exported` | The trace was kept and queued to at least one exporter, which sends it right after |

### API docs: `GET /docs` and `GET /openapi.yaml`

`/openapi.yaml` serves the OpenAPI 3.0 description of this API. `/docs` renders
it with Swagger UI, where you can also send requests. The UI's scripts load
from a CDN, so `/docs` needs internet access in the browser; the spec does not.

The spec lives in `internal/server/openapi.yaml` and is embedded in the binary.
Update it when you change a handler; a test fails if a route, a documented
status code or a field of the decision response goes missing from it.

### `GET /healthz` and `GET /readyz`

`/healthz` returns 200 whenever the process is up. `/readyz` returns 200 only
if the queue is up (the NATS connection, when NATS is used) and the default
model (the `scorer` section) answers `GET /v1/models` on its host within 2
seconds, and 503 otherwise. An agent's own model is not checked: when it is
down, only that agent's traces are affected (retried, then dead-lettered),
so it does not take the engine out of a load balancer; watch
`tailwise.scorer.duration` with `error.type` per `tailwise.model` for it.

## How a decision is made

1. **Route.** The trace's agent picks its model (see
   [One model per agent](#one-model-per-agent)).
2. **Slice.** `trace.BuildState` renders the trace into the text the model
   reads (see [The slice](CONTRIBUTING.md#the-slice-and-its-golden-files)).
3. **Score.** The slice and the model's questions (its manifest's, or the
   built-in ones) go to the model, in one call. A queued trace that hits a system error goes back to the
   queue for redelivery. On the synchronous endpoint, which has no queue,
   the engine retries it with `scorer.max_retries` and jittered
   `scorer.backoff`. The two kinds of retry never multiply.
4. **Decide.** `p_keep` combines the model's yes/no answers with the
   manifest's weights (see [Combining the answers](#combining-the-answers)),
   or, for a manifest without them, is its answer to the decision question.
   - `p_keep >= keep_threshold`: keep, rule `above_threshold`.
   - Otherwise keep with probability `random_floor`, rule `random_floor`.
   - Otherwise drop, rule `below_threshold`.
5. **Export.** Kept traces are queued to every exporter. Each decision produces one JSON log
   line with `trace_id`, `agent`, `decision`, `rule`, `reason`, `cause`, `p_keep`, `model`, `latency_ms`,
   and `answers`: every yes/no answer, by question, as in
   `"answers": {"answer_wrong": 0.91, "unsupported_answer": 0.84, ...}`.

### Reasons

Every decision says in plain English why the model thinks the final answer
is wrong. Jev answers with probabilities and choices, not free text, so the
reason comes from a choice question, `why_wrong` in the built-in questions
(`scorer.reason_question` names it). Each option is a sentence:

| `cause` | `reason` |
| --- | --- |
| `correct` | The final answer is correct, complete and supported by the evidence. |
| `contradicts_evidence` | The final answer contradicts the evidence the agent gathered. |
| `unsupported` | The final answer makes claims the evidence does not support. |
| `incomplete` | The final answer leaves out part of what the question asked. |
| `wrong_lookup` | The agent looked up the wrong thing, so its evidence does not answer the question. |
| `ignored_tool_failure` | A tool call failed and the agent answered anyway without the information it needed. |
| `no_answer` | The agent did not give an answer. |

The model picks the option that fits, and its sentence becomes the reason.
A manifest can define its own options in the same shape, with sentences
that suit your agent. The reason question rides along in the same request
as the decision question, so it costs no extra call. If a manifest has no
reason question, the reason says only how likely the answer is wrong, and
the engine warns at start-up. On fallback it says the model could not score
the trace.

### When the model cannot score a trace

What happens depends on whose fault it is and which path the trace took.

| Failure | Queued (`POST /v1/traces`) | Sync (`:score`) |
| --- | --- | --- |
| **System error**: the model unreachable, timing out, 5xx, 401/403/404/429, or an unparseable response | Redelivered, then dead-lettered. No decision is made. | Retried, then a random fallback decision |
| **The model rejects the trace**: 400, 413 or 422 | Random fallback decision | Random fallback decision |

The random fallback keeps the trace with probability `random_floor` and drops
it otherwise, with rule `fallback` either way.

## The queue

### Where the queues live

The engine has three kinds of queue: the trace work queue with its
dead-letter stream, the store where OTLP spans wait to be assembled, and one
export queue per exporter. `queue.url` decides where all of them live:

| `queue.url` | NATS server | Survives a restart | Shared by replicas |
| --- | --- | --- | --- |
| empty (the default) | embedded in the engine, no port opened | only with `queue.store_dir` set | no, each engine works alone |
| `nats://...` or `tls://...` (`TAILWISE_NATS_URL`) | external | yes | yes, through the durable consumers |

Both run the same JetStream code, so the rules are the same: a bounded
queue (`queue.max_messages`, `exporters[].queue_size`), redelivery with
backoff up to a delivery limit, then a dead letter stored in a DLQ stream;
spans assembled after `otlp.idle_timeout` of quiet or `otlp.max_wait`. The
embedded server suits trying the engine and single-instance setups; use an
external server to share the work between replicas. With the embedded
server `queue.replicas` must be 1. Docker Compose sets `TAILWISE_NATS_URL`,
so the compose stack uses its own NATS container.

The rest of this section applies to both.

- **Stream** `TAILWISE_TRACES` uses work-queue retention on file storage: a
  trace is removed once a worker acknowledges it. A full stream refuses new
  traces rather than evicting unscored ones.
- **Workers** pull one trace at a time through the durable consumer
  `tailwise-workers`. Run more engine replicas with the same consumer name to
  share the work.
- **Delivery is at least once.** If a worker dies after exporting a trace but
  before acknowledging it, the trace is scored and exported again.
- **Payload** is the trace JSON, the same shape the API accepts, so another
  producer can publish straight to `tailwise.traces`. A trace published that
  way without a `trace_id` gets one derived from its stream sequence.
- A trace assembled from OTLP spans also carries those spans in an
  `otlp_spans` field (base64 protobuf), so a kept trace can be forwarded as
  the originals. `nats.conf` raises the NATS message limit to 8 MB for this.
  A trace still too big is queued without its spans and, if kept, exported
  as rebuilt spans instead.

### NATS names

Every stream, subject and consumer is named from `queue.prefix`. With the
default, `tailwise`:

| What | Stream | Subject |
| --- | --- | --- |
| Traces waiting to be scored | `TAILWISE_TRACES` | `tailwise.traces` |
| Traces that failed for good | `TAILWISE_TRACES_DLQ` | `tailwise.traces.dlq` |
| Spans waiting to be assembled | `TAILWISE_SPANS` | `tailwise.spans.<trace id>` |
| One marker per trace awaiting assembly | `TAILWISE_PENDING` | `tailwise.pending.<trace id>` |
| Each exporter's queue | `TAILWISE_EXPORTS_<NAME>` | `tailwise.exports.<name>` |
| Exports given up on | `TAILWISE_EXPORTS_DLQ` | `tailwise.exports_dlq` |

The workers pull from the consumer `tailwise-workers`, and each exporter's
workers from `tailwise-workers-export`. Engines with the same prefix share
the work, so give each deployment on one NATS server its own prefix, for
example `staging`.

### NATS in production

The engine is a NATS client; it does not run or configure the server
(`nats.conf` and the compose service are only for trying it). Against a
secured or clustered NATS, set:

```yaml
queue:
  url: tls://nats.internal:4222
  replicas: 3                                  # every stream the engine creates, kept on 3 servers
  auth:
    credentials_file: /var/run/secrets/nats/tailwise.creds   # or nkey_seed_file, user + password_env, token_env
  tls:
    ca_file: /var/run/secrets/nats/ca.pem      # a private CA
    # cert_file / key_file                     # mutual TLS
```

`replicas` is applied to every stream the engine creates (traces, dead
letters, spans, pending markers, each exporter's queue); more than the
cluster has servers fails at start-up. An existing stream is updated to the
new count when the server allows it. Passwords and tokens come from
environment variables (`password_env`, `token_env`); credential, seed and
certificate files from paths, relative to the config file, typically mounted
from a Kubernetes secret. The account needs JetStream access to the streams
under `queue.*` and `otlp.*` and the right to create durable consumers.

### Streams

| Stream | Holds | Removed when |
| --- | --- | --- |
| `TAILWISE_SPANS` | OTLP spans, one subject per trace ID | The trace is assembled, or after `otlp.span_max_age` |
| `TAILWISE_PENDING` | One marker per trace awaiting assembly | The trace is assembled |
| `TAILWISE_TRACES` | Traces waiting to be scored | A worker acknowledges the trace |
| `TAILWISE_TRACES_DLQ` | Traces that hit system errors | After `queue.dlq_max_age` |
| `TAILWISE_EXPORTS_<NAME>` | Kept traces waiting for one exporter (its name upper-cased, `-` as `_`) | The exporter sends the trace, or gives up on it |
| `TAILWISE_EXPORTS_DLQ` | Kept traces an exporter gave up on, with headers `Tailwise-Exporter`, `Tailwise-Dead-Error`, `Tailwise-Deliveries` | After `queue.dlq_max_age` |

### Redelivery and the dead-letter stream

When a worker hits a system error (one call per delivery), it returns the
trace to the queue with a delay from
`queue.redelivery_backoff`. After `queue.max_deliver` deliveries it copies the
trace to `TAILWISE_TRACES_DLQ` and removes it from the work queue. With the
defaults that is 5 deliveries over about 8 minutes.

A message that is not a valid trace is dead-lettered on first sight.

A dead letter's body is the original trace. Its headers say what happened:

| Header | Example |
| --- | --- |
| `Tailwise-Trace-Id` | `wrong-river-001` |
| `Tailwise-Dead-Reason` | `system_error` or `invalid_payload` |
| `Tailwise-Dead-Error` | `scorer: score trace: model returned HTTP 503: ...` |
| `Tailwise-Deliveries` | `5` |
| `Tailwise-Dead-At` | `2026-10-12T08:42:48Z` |
| `Tailwise-Source-Stream`, `Tailwise-Source-Sequence` | `TAILWISE_TRACES`, `17` |

Each dead-lettered trace also produces an `ERROR` log line, `trace dead-lettered`.

Inspecting and replaying, with the compose stack up:

```sh
make dlq      # how many dead letters there are

# The NATS CLI, run from a container on the compose network:
alias nats='docker run --rm -i --network tailwise_default natsio/nats-box:latest nats -s nats://nats:4222'

nats stream get TAILWISE_TRACES_DLQ 1          # one dead letter, with headers

# Replay it once Jev is back: post its body to the API again.
nats stream get TAILWISE_TRACES_DLQ 1 --json | jq -r .data | base64 -d \
  | curl -X POST localhost:8080/v1/traces -d @-

nats stream purge TAILWISE_TRACES_DLQ          # clear the dead letters
```

NATS monitoring is at http://localhost:8222 (`/jsz?streams=true` for stream
and consumer counts).

## Exporters

Kept traces go to every back end in `exporters`, over OTLP/HTTP. The default
is the local Phoenix:

```yaml
exporters:
  - name: phoenix
    endpoint: http://localhost:6006/v1/traces
    resource_attributes:
      openinference.project.name: tailwise   # the Phoenix project
```

Add more entries to send to more back ends. Langfuse, for example, takes
Basic auth built from its public and secret keys
([`examples/langfuse.yaml`](examples/langfuse.yaml)):

```yaml
  - name: langfuse
    endpoint: https://cloud.langfuse.com/api/public/otel/v1/traces
    headers_env: LANGFUSE_OTLP_HEADERS
```

```sh
export LANGFUSE_OTLP_HEADERS="Authorization=Basic%20$(printf '%s' "pk-lf-...:sk-lf-..." | base64)"
```

**Headers.** An exporter's headers are its `headers`, plus those parsed from
the variable `headers_env` names, which win on a name clash. The variable
uses the OpenTelemetry format: `key=value` pairs separated by commas, values
percent-encoded where needed (`%20` for a space). The engine logs exporter
names and endpoints, never header values, and a header that fails to parse
is reported without its value.

**Asynchronous.** Exporting never holds up scoring. Each exporter has its own
bounded queue (`queue_size`) and its own workers (`concurrency`); a kept
trace is queued to every exporter and the worker moves on. The queue is in
NATS JetStream when `queue.url` is set (stream `TAILWISE_EXPORTS_<NAME>`, so
queued exports survive a restart, unless the embedded server runs without
`queue.store_dir`); see [Where the queues live](#where-the-queues-live). A slow or unreachable back
end only fills its own queue: past `queue_size`, traces are skipped for that
back end alone and each skip is reported to the error sinks (`kind=full`,
`exporter=<name>`). The decision log's `exported` field says whether at least
one exporter queued the trace.

**Retries.** A send returns only once the back end has answered, so a
failure is never silent. A failed send is retried after `backoff` (default
1s, 5s, 30s), up to `max_deliver` tries (default 3); each retry is reported.
Then the exporter gives up: the trace is reported as a severe error and
moved to `TAILWISE_EXPORTS_DLQ` with the exporter's name, from where it can be
replayed. An error a retry cannot fix (corrupt stored spans) is given up at
once. Export failures never send the trace to the trace dead-letter stream:
it was scored, only its delivery failed.

**Shutdown.** With the embedded server and no `queue.store_dir`, the engine
first sends what is queued, within 10 seconds, since the queue goes away
with the process. Otherwise it stops after the sends in flight and the rest
stays queued for the next start.

### What the back ends receive

The engine writes [OpenInference](https://github.com/Arize-ai/openinference)
attributes, which Phoenix reads natively and Langfuse and other OTLP back
ends show as span attributes, plus its own `eval.*` and `tailwise.*`
attributes.

**A kept trace that arrived over OTLP** is forwarded as its own spans: same
trace ID, span IDs, names, timings and attributes. Two things change: the
exporter's `resource_attributes` are set on every resource (replacing
attributes of the same name), and the root span gains the decision
attributes below. The agent's own `service.name` is kept.

**A kept trace from the JSON API** has no spans of its own, so the engine
builds them, as a new trace with a generated trace ID. Resource:
`service.name=tailwise` plus the exporter's `resource_attributes`.
A root span `agent_run`, and one child span per tool call, named after the
tool.

Root span attributes:

| Attribute | Type | Set on | Value |
| --- | --- | --- | --- |
| `openinference.span.kind` | string | rebuilt | `AGENT` |
| `input.value` | string | rebuilt | The question |
| `output.value` | string | rebuilt | The final answer |
| `tailwise.source_trace_id` | string | rebuilt | The client's `trace_id` |
| `tailwise.evidence.<i>` | string | rebuilt | Each evidence item, 0-based, untruncated |
| `eval.agent` | string | both | The trace's agent; empty if it named none |
| `eval.decision` | string | both | `keep` (dropped traces are not exported) |
| `eval.rule` | string | both | Which part of the policy kept it: `above_threshold`, `random_floor`, or `fallback` (the model could not score it) |
| `eval.reason` | string | both | Why the model thinks the final answer is wrong, in plain English |
| `eval.cause` | string | both | The model's short name for the reason, e.g. `contradicts_evidence`; omitted when it gave none |
| `eval.model` | string | both | The scorer model that decided |
| `eval.latency_ms` | double | both | Scoring time in milliseconds |
| `eval.p_keep` | double | both | The model's probability that the answer is wrong; omitted on a fallback keep |

Tool span attributes (rebuilt traces only; forwarded tool spans are not
changed):

| Attribute | Type | Value |
| --- | --- | --- |
| `openinference.span.kind` | string | `TOOL` |
| `tool.name` | string | The tool's name |
| `input.value` | string | Its arguments, as compact JSON; omitted when there are none |
| `output.value` | string | Its status |

Resource attributes worth setting per back end:

| Attribute | Back end | Effect |
| --- | --- | --- |
| `openinference.project.name` | Phoenix | The project the traces are filed under |
| `deployment.environment` | any | Lets you filter production from staging |
| `service.version` | any | The agent version the traces came from |

Rebuilt and forwarded spans alike are sent in one request per trace, with a
10-second timeout. A failed send is retried by the export queue only
(`exporters[].max_deliver` and `backoff`), not inside the request.

## Metrics

With `observability.metrics.endpoint` set, the engine sends its own
OpenTelemetry metrics over OTLP/HTTP every `interval`, to a Collector or any
back end that takes OTLP metrics:

```yaml
observability:
  metrics:
    endpoint: http://otel-collector:4318/v1/metrics
    headers_env: METRICS_OTLP_HEADERS     # optional, "key=value,..."
    resource_attributes:
      deployment.environment: production
```

| Instrument | Kind | Attributes | Measures |
| --- | --- | --- | --- |
| `http.server.request.duration` | histogram (s) | `tailwise.server` (`api`, `otlp`), `http.request.method`, `http.route`, `http.response.status_code` | JSON API and OTLP/HTTP requests |
| `rpc.server.call.duration` | histogram (s) | `tailwise.server`, `rpc.system`, `rpc.method`, `rpc.grpc.status_code` | OTLP/gRPC calls |
| `tailwise.scorer.duration` | histogram (s) | `tailwise.agent`, `tailwise.model` | Model calls, retries included |
| `tailwise.export.duration` | histogram (s) | `tailwise.exporter` | Sends to a trace back end |
| `tailwise.decisions` | counter | `tailwise.agent`, `tailwise.model`, `tailwise.decision`, `tailwise.rule`, `tailwise.cause` | Decided traces, so also what kinds of failure the model finds |
| `tailwise.errors` | counter | `tailwise.component`, `error.type` | Errors reported to the error sinks |
| `tailwise.queue.depth` | gauge | `tailwise.queue` (`traces`, `export:<exporter>`) | Traces waiting: to be scored, or to be sent to a back end. On a shared NATS every pod reports the shared count; on embedded NATS, its own |

`tailwise.agent` is bounded: an agent with its own model under
`agents.models` appears by name, a trace naming any other agent as `other`,
and one naming none as `none`. Agent names come from the traces, so any
client could otherwise create unlimited metric series. The decision log
uses the same label; the trace's own agent name stays on its exported spans
(`eval.agent`) and in its `/v1/traces:score` response, cut to 128 bytes.

A failed request or call also carries `error.type`: the status code (5xx for
HTTP, any non-OK gRPC code), or the error kind (`unavailable`, `timeout`,
`rejected`, ...) for model calls and sends. An error rate is the count with
`error.type` over the total count; a latency percentile comes from the
histogram. The resource has `service.name=tailwise` plus
`resource_attributes`. As with exporters, the global
`OTEL_EXPORTER_OTLP_HEADERS` is not read. The last metrics are sent on
shutdown.

## Deploying

Run the engine inside your private network (a VPC), next to the agents that
send it traces, and control access there: security groups or firewall rules,
a Kubernetes NetworkPolicy, and IAM roles or service accounts for whatever
the engine itself calls. The engine does not authenticate clients on its JSON
API or OTLP receivers, and serves them without TLS; put a load balancer or
service mesh in front of it if traffic must be encrypted or authenticated
in transit.

What leaves the network is what you configure: exporters (with their own
headers), the metrics endpoint, and a hosted scorer such as Jev, which
receives every scored trace's text.

On Kubernetes, install with the Helm chart in
[`deploy/helm/tailwise`](deploy/helm/tailwise/README.md): the engine, an
optional NATS JetStream cluster, and an optional trace-routing gateway for
several replicas with queues on embedded NATS. Kustomize can install the same chart.

Before a rollout, `tailwise -check -config my-config.yaml` (or plain
`tailwise -check` for the built-in defaults) loads and validates the
configuration and its manifests, checks the secrets it names are set, prints
any warnings, and exits: 0 if valid, 1 with the problems if not.

## Known limitations

These are open problems in the current version, not design choices to rely on.

- **Dead letters are not replayed automatically.** A Jev outage longer than
  the redelivery window (about 8 minutes with the defaults) sends every queued
  trace to `TAILWISE_TRACES_DLQ`, and they stay there until someone replays
  them by hand (see [Redelivery and the dead-letter stream](#redelivery-and-the-dead-letter-stream)).
  Nothing alerts on the dead-letter count either; watch for `trace dead-lettered`
  log lines or run `make dlq`.
- **A model that rejects everything causes silent random decisions.** A 400,
  413 or 422 is treated as that trace's problem and gets the random fallback.
  If the model starts answering that way for every request (for example after
  a change to its request schema), every trace is decided at random, about
  98% dropped with the default `random_floor`, and only `WARN` log lines show
  it.
- **The built-in weights are a reasonable guess, not fitted.** Nothing tunes
  them, or the model's probabilities, to your agent, so `keep_threshold` may
  keep too much or too little at first. Watch the share of traces kept and
  adjust the threshold; with labelled traces, fit the weights (see
  [Combining the answers](#combining-the-answers)).
- **Every scored trace is a billed call to Jev, and leaves your network.**
  Nothing caps the spend or redacts fields before sending; only
  `server.workers` limits the rate.
- **An exporter can lose kept traces.** A trace skipped because the
  exporter's queue was full is not sent to that back end. One it gave up on
  after `max_deliver` tries waits in `TAILWISE_EXPORTS_DLQ`.
- **The embedded server without `queue.store_dir` loses queued work on a
  crash.** Traces waiting to be scored, spans waiting to be assembled and
  traces waiting to be exported are in a temporary directory. Set
  `queue.store_dir`, or use an external NATS (`queue.url`), where that
  matters.
- **Plain instrumentor spans do not reproduce the training slice exactly.**
  See [Matching what the model was trained on](#matching-what-the-model-was-trained-on).
  The test agent does not set the `tailwise.*` attributes yet, so its traces
  are scored on a slightly different slice than the model saw in training.
  On the one run compared, the raw score moved from 0.259 to 0.266.
- **Trace completeness is a guess.** A trace is assembled after
  `otlp.idle_timeout` of silence. Spans that arrive later are assembled as a
  separate, partial trace, which is normally unscorable and discarded; the
  first decision is not revisited. An agent run that pauses for longer than
  the idle timeout between spans is split this way.
- **The API and the OTLP receivers have no authentication or TLS.** Anyone
  who can reach 8080, 4317 or 4318 can submit traces. This is by design; see
  [Deploying](#deploying).
- **A trace can be scored and exported twice.** Delivery is at least once and
  nothing deduplicates: a worker that dies between exporting and acknowledging
  leaves a duplicate in the back ends. The same happens if a client retries a
  `POST /v1/traces` that had in fact succeeded.
- **The compose file's NATS has no authentication.** It is there to try the
  engine, bound to loopback. In production, point `queue.url` at a NATS you
  run or buy, with `queue.auth`, `queue.tls` and `queue.replicas` (see
  [NATS in production](#nats-in-production)).

## Shutdown

On SIGINT or SIGTERM the engine stops accepting requests and OTLP exports,
stops pulling from the queue, and lets in-flight traces finish for up to
`server.shutdown_timeout`. Spans not yet assembled and traces still queued
stay in JetStream for the next start. If the timeout runs out, in-flight model calls are cancelled and those
traces go back to the queue; they are not dead-lettered and get no fallback
decision. Last, the exporters send the traces still queued for them and
shut down (up to 10 seconds in all).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the code layout, tests, the golden
slice files, commit conventions, releases and every `make` target.
