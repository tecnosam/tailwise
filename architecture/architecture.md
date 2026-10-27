# Architecture of the inference engine

How the Go service in this directory is put together: the stack, the code
layout, the design patterns it uses and the ones it deliberately does not,
and how `tailwise.yaml` becomes the running configuration.

The [README](../README.md) covers what the engine does and how to run it.
This file is about the code. Diagrams of it, kept beside this file:

- [Sequence diagrams](sequence-diagrams.md): a trace from intake to export,
  package by package, with the values passed at each step
- [Package dependency graph](dependency-graph.md): which package imports which
- [Class diagrams](class-diagrams.md): the structs and interfaces, and how they
  implement and hold one another

## Short answers

| Question | Answer |
| --- | --- |
| Web framework? | None. The standard library's `net/http` and its `ServeMux`. No Gin, Echo, Fiber or chi. |
| Dependency injection? | Yes, by hand: constructors take what they need, and `main` builds everything once. No DI framework or container. |
| Factory pattern? | None. Where something is built from settings and tests need to swap it, the builder is a plain function type: `export.BackendFunc` (e.g. `otlpbackend.Build`) and `export.QueueFunc` (from `broker.ExportQueues`). Elsewhere, constructor functions (`scorer.New`, `pipeline.NewPolicy`). |
| How are dependencies swapped in tests? | Small interfaces declared by the package that uses them, plus a few function-typed fields. |
| Config? | `config.Read` decodes the YAML strictly over a struct of defaults (`gopkg.in/yaml.v3`); `config.Build` applies six environment overrides, validates, reads secrets and manifests, and maps the file onto each component's own settings. Read once at start-up. |
| Database? | None. The only state is the queues and the spans waiting to be assembled: in NATS JetStream, on the server `queue.url` names or on one embedded in the engine. |
| Logging? | `log/slog`, JSON to stdout, for events (start-up, decisions, shutdown). Errors go through their own pipeline: typed `fault.Error`s reported to `observe`, which writes them to the configured sinks. |

## Stack

| Concern | What is used |
| --- | --- |
| Language | Go 1.26, one static binary (`CGO_ENABLED=0`) |
| JSON API | `net/http` with `http.ServeMux` method patterns (`"POST /v1/traces"`), available since Go 1.22 |
| OTLP over HTTP | A second `net/http` server on its own port |
| OTLP over gRPC | `google.golang.org/grpc`, serving the standard OTLP `TraceService` |
| Wire formats | `google.golang.org/protobuf` and the generated OTLP types in `go.opentelemetry.io/proto/otlp` |
| Queue and storage | NATS JetStream through `github.com/nats-io/nats.go` (the `jetstream` package), on an external server or on one embedded with `github.com/nats-io/nats-server/v2` when no NATS URL is set |
| Calling the model | `net/http` client, JSON, to any System One model (Jev by default) |
| Sending to trace back ends | The OpenTelemetry Go SDK with the `otlptracehttp` exporter, one per configured back end (Phoenix, Langfuse, any OTLP/HTTP endpoint) |
| Config | `gopkg.in/yaml.v3` for `tailwise.yaml`, `encoding/json` for the model manifest |
| API docs | A hand-written OpenAPI file embedded with `go:embed`; Swagger UI loaded from a CDN |
| Tests | The standard `testing` package, `net/http/httptest`, and a real NATS server started in-process (`github.com/nats-io/nats-server/v2`, also the engine's embedded server) |

There is no ORM, no router library, no validation library, no CLI library
(two flags, read with `flag`) and no mocking library.

## Directory structure

```
cmd/tailwise/main.go     the only executable: flags, config, wiring, shutdown
internal/                every package; none is importable from another module
  pipeline/              the core: domain types, the interfaces the engine needs, model routing, the keep/drop policy, and Engine
  trace/                 the Trace record (with its agent), its JSON decoding, and BuildState (the text the model reads)
  fault/                 the error type: kind, component, operation, trace; constructors and classifiers
  observe/               where errors go: Reporter, the error pipeline, sinks (stdout, file, otlp)
  metrics/               the engine's own OpenTelemetry metrics: Recorder, its OTLP pipeline, and Noop
  retry/                 the one retry policy: attempts, backoff, jitter
  config/                the YAML file (File), the engine's settings (Engine), validation, manifests
  scorer/                model adapter: /v1/systemone client and the per-agent model builder
  broker/                the JetStream connection: an external NATS server, or one embedded in the process
  queue/                 the trace work queue and span store: AbstractBroker, Config, Handler, the shared rules
    natsq/               on NATS JetStream: the queue, its workers, span assembly
    queuetest/           an in-process NATS server and helpers for tests
  export/                sends kept traces to every back end: Dispatcher, the backend and queue contracts
    otlpbackend/         the OTLP/HTTP back end (Phoenix, Langfuse, any OTLP endpoint)
    natsq/               export queues on NATS JetStream
  server/                JSON API handlers: HTTP in, pipeline calls, HTTP out
  otlp/                  OTLP gRPC and HTTP receivers; turning spans into a Trace
  jsq/                   the one generic JetStream work queue both NATS queues are built on
  common/                what several packages share: the NATS connection and pull loop, a worker
                         pool, HTTP request metrics, small helpers
testdata/                trace fixtures, golden state strings, canned model responses
scripts/                 send_samples.py and the sample traces it sends (samples.jsonl)
tailwise.yaml            a local test config; the engine needs none, and the image bakes none in
tailwise.schema.yaml     JSON Schema for tailwise.yaml
```

Everything lives under `internal/`, so Go refuses imports from any other
module. Tailwise is a binary, not a library: renaming a package or changing a
signature is never a breaking change, and the release version tracks only
what users see (the config file, the HTTP API, the exported attributes).
Retries everywhere use one type, `retry.Policy`.

### Which package imports which

```
fault, retry, trace   (nothing of the engine's)
observe  -> fault
pipeline -> fault, observe, retry, trace           the core, with the keep/drop policy
metrics  -> fault, pipeline
common   -> fault, observe, metrics                internal helpers
jsq      -> common, fault, observe, retry          the generic JetStream work queue
scorer   -> fault, pipeline, retry, trace          adapters implement the core's interfaces
queue, natsq          -> jsq, common, ...          the trace queue
export, export/natsq, otlpbackend -> pipeline, jsq, otlp, ...
broker   -> common, export, queue and both natsq
otlp, server          -> common, fault, metrics, observe, trace (server also pipeline)
config   -> broker, export, metrics, observe, pipeline, queue, retry, scorer, trace
main     -> all of them
```

The [package dependency graph](dependency-graph.md) has every import, drawn
as layers and in full, with the command that regenerates it.

Three things are worth noticing:

- **The core imports no adapter.** `pipeline` defines what it needs
  (`Scorer`, `ModelRouter`, `Exporter`, `Publisher`, `Metrics`) and the types they
  exchange (`Score`, `Decision`, `Record`). The scorer and the exporter
  import it to implement those, never the other way round, and `server`
  depends on the core alone, not on the queue, scorer or exporter.

- **Only `main` imports `config`.** No other package knows the YAML exists.
  Each package that needs settings declares its own `Config` struct
  (`scorer.Config`, `queue.Config`, `pipeline.PolicyConfig`). `config` builds those
  structs (`engine.go`), so it depends on the components and never the other
  way round. A component can therefore be tested, or reused, without a config
  file.
- **`trace` is the shared vocabulary.** The receivers, the queue, the
  pipeline and the exporter all pass a `trace.Trace` around; none of them
  depends on another's internals to do so.

## How a trace flows through the code

```
JSON  POST /v1/traces        server.handleEnqueue -> queue.Publish ─────────────────────┐
OTLP  gRPC / HTTP            otlp.Receiver -> queue.PublishSpans                         │
                                └─ queue.Assemblers -> otlp.Assembler -> queue.Publish ──┤
                                                                                         v
                                                       JetStream work queue (TAILWISE_TRACES)
                                                                                         │
                             queue.Workers -> pipeline.Engine.ProcessQueued <────────────┘
                                                 ModelRouter.Route(trace.Agent) -> the agent's Model
                                                 trace.BuildState -> Model.Scorer.Score
                                                 -> Policy.Decide
                                                 -> Exporter.Export (kept traces only: queued per back end,
                                                    sent by that back end's workers)

JSON  POST /v1/traces:score  server.handleScore -> pipeline.Engine.Process   (same pipeline, no queue)
```

The [sequence diagrams](sequence-diagrams.md) follow each of these paths call
by call, with the structs passed between packages.

`pipeline.Engine` is the pipeline. Both the synchronous endpoint and the queue
workers call it; the only difference is what happens when the model fails (see
[Error handling](#error-handling)).

## From tailwise.yaml to running configuration

The file and what the engine runs with are two different types, on
purpose. `config.File` is the YAML file's keys as written, with their
defaults; it holds the names of environment variables, never the secrets in
them, and manifest paths, never their contents. `config.Engine` is every
component's settings in the component's own type (`queue.Config`,
`broker.Config`, `scorer.Models`, `export.BackendConfig`, ...), secrets and
manifests already read. One function maps the first to the second, so a key
can be renamed in the file without touching a component, and a component's
settings can change without a new key.

```
internal/config/
  config.go      File: the YAML keys, Default() and DefaultModel()
  load.go        Read (file to File), Build (File to Engine), Load (both)
  engine.go      Engine, and the one mapping from File to each component's settings
  validate.go    the rules a File must meet
  manifest.go    reading and checking manifest.json
  utils.go       small shared helpers
```

```go
eng, err := config.Load("tailwise.yaml")              // what the binary does
f, err := config.Read("tailwise.yaml")                // just the file, decoded over the defaults
eng, err := config.Build(f, dir, getenv)              // a File made or changed in code, any environment
```

`Build` runs in this order:

1. **Apply environment overrides.** Six variables replace their keys when
   set and non-empty: `TAILWISE_NATS_URL`, `TAILWISE_SCORER_ENDPOINT`,
   `TAILWISE_EXPORT_ENDPOINT` (the first exporter's endpoint),
   `TAILWISE_MANIFEST`, `TAILWISE_OTLP_GRPC_ADDR` and
   `TAILWISE_OTLP_HTTP_ADDR`. Nothing else can be set from the environment.
   The environment is a parameter (`os.Getenv` in `Load`), so tests and
   embedding programs control it.
2. **Resolve relative paths** (manifests, `queue.store_dir`, NATS
   credentials and certificates) against the config file's directory.
3. **Validate and read the secrets.** `validate` checks every value, and
   every secret the file names is read from its variable: each model's API
   key (`JEV_API_KEY` by default for `kind: jev`), the NATS password or
   token, and the `headers_env` of each exporter, the metrics exporter and
   each OTLP error sink. All problems are collected and reported together
   (`errors.Join`). Rules span keys, for example `queue.ack_wait` must be
   longer than the slowest model's `timeout`.
4. **Load the manifests.** The engine takes each manifest's `questions`
   object, kept byte-for-byte in its order, and checks that the decision
   question exists and is a `noul`. A model with no manifest gets the
   built-in one (`builtin_manifest.json`, embedded in the binary),
   and `Engine.Warnings` says so; main logs each warning at start-up.

With no file at all, `config.Load("")` builds `config.Default()`: Jev as the
scorer, which needs only `JEV_API_KEY`.

`Read` decodes the file over `config.Default()`, strictly
(`KnownFields(true)`): a misspelt or unknown key stops the engine with its
line number. `run` in `cmd/tailwise/main.go` then hands each component its
part of the `Engine`, adding only runtime dependencies such as the logger:

```go
m, err := metrics.Start(ctx, eng.Metrics)
models, err := scorer.BuildRouter(eng.Models)
b, err := broker.New(eng.Broker)                       // external NATS, or embedded
q, err := b.Ingest(ctx, eng.Queue)
exp, err := export.NewDispatcher(ctx, export.DispatcherConfig{Backends: eng.Exporters, NewBackend: otlpbackend.Build, NewQueue: b.ExportQueues(eng.ExportStreams), ...})
pipeline.NewPolicy(eng.Policy, nil)
```

Details worth knowing:

- **Durations** are written as strings (`10s`, `2m`) and decoded by
  `yaml.v3` straight into `time.Duration`.
- **No reload.** The config is read once. Changing it means restarting the
  engine.
- **No globals.** The config is passed down as arguments; nothing reads it
  from a package-level variable.
- **Flags** sit outside the file: `-config` (optional: without it the
  built-in defaults), `-check` and `-log-level`.

[tailwise.schema.yaml](../tailwise.schema.yaml) describes every key, with its
type, default and limits. A test (`internal/config/schema_test.go`) fails if
the schema and the Go structs disagree on a key or a default.

## Design patterns in use

### Manual dependency injection, with one composition root

Every component receives its collaborators through its constructor or its
struct fields. None of them creates its own dependencies or reaches for a
global. `run` in `main.go` is the one place where concrete types are chosen
and connected:

```go
a.engine = pipeline.NewEngine(pipeline.Deps{
    Models: models, Policy: pipeline.NewPolicy(...), Exporter: a.exporter, Reporter: a.reporter, Metrics: a.metrics,
})
srv := server.New(server.Options{Engine: a.engine, Queue: a.queue, Reporter: a.reporter, Metrics: a.metrics})
```

`main` builds them in small steps on an `app` struct (`startMetrics`,
`startReporter`, `startBroker`, `startQueue`, `startExporter`,
`startEngine`, ...), so a failure part-way
stops exactly what was started.

There is no DI framework (no Wire, Fx or Dig). With about ten objects, wiring
by hand is shorter than configuring a container, and the start-up order is
readable top to bottom.

### Interfaces defined by the consumer

A package declares an interface for what it needs, next to the code that
uses it, and the concrete type lives elsewhere. All of them are small:

| Interface | Declared in | Methods | Real implementation |
| --- | --- | --- | --- |
| `pipeline.Scorer` | `pipeline/interfaces.go` | `Score`, `Ready` | `*scorer.Client` |
| `pipeline.ModelRouter` | `pipeline/model.go` | `Route`, `Ready` | `*pipeline.AgentRouter` |
| `pipeline.Exporter` | `pipeline/interfaces.go` | `Export` | `*export.Dispatcher` |
| `export.AbstractBackend` | `export/backend.go` | `Send`, `Shutdown` | `*otlpbackend.Backend` |
| `pipeline.Publisher` | `pipeline/interfaces.go` | `Publish`, `Ready` | `queue.AbstractBroker` |
| `queue.AbstractBroker` | `queue/broker.go` | `Publish`, `PublishSpans`, `StartWorkers`, `StartAssemblers`, `Ready`, `Close` | `*natsq.Queue` (JetStream) |
| `export.AbstractQueue` | `export/queue.go` | `Enqueue`, `Flush`, `Shutdown` | the JetStream export queue |
| `pipeline.Metrics` | `pipeline/interfaces.go` | `ScoreFinished`, `TraceDecided` | `*metrics.Recorder`, `metrics.Noop` |
| `export.SendMetrics` | `export/dispatcher.go` | `SendFinished` | `*metrics.Recorder`, `metrics.Noop` |
| `common.RequestMetrics` | `common/httpmetrics.go` | `RequestFinished` | `*metrics.Recorder`, `metrics.Noop` |
| `observe.ErrorCounter` | `observe/observe.go` | `ErrorReported` | `*metrics.Recorder`, `metrics.Noop` |
| `observe.Reporter` | `observe/observe.go` | `Report` | `*observe.Pipeline` |
| `otlp.Sink` | `otlp/receiver.go` | `PublishSpans` | `queue.AbstractBroker` |
| `otlpbackend.Uploader` | `export/otlpbackend/backend.go` | `UploadTraces` | the OTLP/HTTP client |

The [class diagrams](class-diagrams.md) draw each of these interfaces with the
structs that implement and hold them.

This is what lets `otlp` store spans in the queue without importing the
`queue` package, and lets the tests replace the model or a trace back end with a
few lines of fake.

### Functions as injection points

Where an interface would have a single method, a function type is used
instead:

- `queue.Handler`: what a worker does with a trace. `main` passes
  `engine.ProcessQueued`.
- `queue.AssembleFunc`: how spans become a trace. `main` passes
  `otlp.Assembler{AgentAttributes: ...}.Assemble`. This keeps `queue` free
  of any OTLP knowledge.
- `export.SendFunc`: how a queued record reaches its back end. The
  dispatcher wraps each back end's `Send` once, to time it for metrics, so
  both queue implementations report the same way.
- `pipeline.NewPolicy(cfg, rnd)`: the random source, so tests can make the random
  floor deterministic.

### Constructor functions

Each component has a `New`-style function that validates its input, fills in
defaults for optional fields (a nil logger becomes `slog.Default()`, a nil
HTTP client a new one) and returns a ready value: `scorer.New`,
`pipeline.NewPolicy`, `server.New`, `otlp.NewReceiver`, `export.NewDispatcher`.
These are plain constructors: each of these components has one
implementation, so there is nothing to choose between yet.

### One model per agent: a router in the core

An agent can have a model of its own: its own questions,
endpoint or key. A trace carries
its agent's name (`trace.Agent`): `otlp.Assembler` reads it from the span
attributes `agents.attributes` lists, the JSON API from the trace's `agent`
field. `pipeline.Engine` asks its `ModelRouter` for the trace's
`pipeline.Model` (a scorer, its name, its retry policy) and scores with
that. `pipeline.AgentRouter` maps agent names to
models with a default for everything else; `pipeline.SingleModel` is the
one-model case.

The models are built from settings in two steps that keep `config` out of
the runtime: `config.Build` turns the scorer section and each `agents.models` entry into
`scorer.Models`, and `scorer.BuildRouter` builds a `/v1/systemone` client
for each one and returns the router. A model server with another protocol
is another `pipeline.Scorer`.

### Metrics through small hooks

Every package that has something worth measuring declares a one-method
hook for it: `pipeline.Metrics` (model calls, decisions),
`export.SendMetrics` (sends to a back end), `common.RequestMetrics`
(requests the API and the OTLP receivers answer, recorded by
`common.InstrumentHTTP` and the receiver's gRPC interceptor), and
`observe.ErrorCounter` (reported errors). `metrics.Recorder` implements all
of them on an OpenTelemetry meter; `metrics.Start` wires it to an OTLP/HTTP
exporter, or to a no-op meter when no endpoint is configured, and
`metrics.Noop` stands in for tests. Each hook takes a small struct
(`metrics.Request`, `metrics.Send`, `pipeline.ScoreResult`) rather than a
list of arguments.

### Configuration sources

Settings can come from the YAML file the binary reads (`config.Read`) or
from a `config.File` a program builds in code; both go through the same
`config.Build`, so they are validated alike. A new source (flags, a
database, a remote store) only has to produce a `File`.

### Trace back ends

`export.Dispatcher` is the engine's `pipeline.Exporter`. It is given a list
of `export.BackendConfig`s and an `export.BackendFunc`, builds one
`export.AbstractBackend` per entry, and gives each its own bounded queue and
workers. `Export` only enqueues, so a slow back end never holds up scoring
or the others; a full queue skips the trace for that back end and reports
it. `otlpbackend.Build` builds an `otlpbackend.Backend` for any OTLP/HTTP
endpoint, which is why Phoenix and Langfuse need no code of their own. A back
end that does not speak OTLP is a new `AbstractBackend` and its build
function, with no change to the dispatcher or the pipeline. Tests pass a
function that hands
out a fake back end. `Send` returns only once the back end has answered, for
forwarded and rebuilt spans alike, so the export queue's retries and dead
letters cover every kept trace.

### One broker, two ways to reach it

`broker.New` opens the one JetStream connection (`common.Conn`) that the
trace work queue, the span store and the export queues share. With
`queue.url` set it dials that server. With it empty it starts a NATS server
inside the process (`DontListen`, so no port is opened) and connects to it
in memory with `nats.InProcessServer`. Its streams go in `queue.store_dir`,
or in a temporary directory removed on close. Both cases run the same
`queue/natsq` and `export/natsq` code, so there is one implementation of the
queue rules (bounded queues, redelivery with backoff, a delivery limit, then
a dead letter), not one per broker.

### Config structs

Components take a `Config` or `Options` struct rather than a long argument
list. Where a test needs to swap a dependency, it is a parameter
(`config.Build` takes the environment lookup) or a small interface.

### One package per implementation

Implementations live in their own package under the one that defines the
interface: `queue/natsq` under `queue`, `export/natsq` and
`export/otlpbackend` under `export`. The parent holds the contract and the
shared rules (`queue.SequenceID`), and its conformance suite runs against
both an external and an embedded NATS server. A parent never imports its
implementations; `broker` builds them.

Both NATS queues are the same generic work queue, `internal/jsq`
(`jsq.Queue[T]`): the stream and its dead-letter stream, the durable
consumer, the worker pool, and the rule for each delivery (acknowledge,
return it with a delay from its `retry.Policy`, or dead-letter it with
headers saying why). The trace queue is a `jsq.Queue[trace.Trace]` plus span
assembly; each exporter's queue is a `jsq.Queue[pipeline.Record]` that gives
up at once on an error a retry cannot fix. They differ only in their
payload and in the words in their reports.

### Pipeline

`pipeline.Engine` runs a fixed sequence, each step its own small method
(`decide`, `scored`, `fallback`, `logDecision`): build the state string,
score it, decide, export, log. Each step is a call into a package that
knows nothing about the others. The steps are not pluggable; the order is
the product.

### Worker pool and competing consumers

`queue.Workers` is a fixed pool of goroutines, each pulling one trace at a
time from a single durable JetStream consumer. Several engine processes
using the same consumer name share the work, and each trace goes to exactly
one of them. `queue.Assemblers` is a second pool of the same shape for
span assembly, and each exporter's queue has a third. With the embedded
server only one process takes part.

### Store first, then acknowledge

Intake answers only after JetStream has confirmed the write: `202` on
the JSON API, success on OTLP. A worker acknowledges a trace only after it has a
decision. A crash at any point therefore leaves the trace in JetStream to be
picked up again, never lost between two steps. The cost is that a trace can
be processed twice; it is never processed zero times. The embedded server
without `queue.store_dir` keeps this guarantee only while the process runs.

### Trace assembly by marker

Spans of one run arrive in several batches. Each batch is stored under
`tailwise.spans.<trace id>`, and one marker per trace is left on a second
stream. An assembler picks up the marker, checks how long the trace has been
quiet, and either puts the marker back with a delay or assembles the trace,
queues it and removes the spans it read. The wait is held in NATS, not in
memory, so it survives a restart.

### Error handling

Every error a component returns is a `*fault.Error`: a **kind** (`invalid`,
`rejected`, `unavailable`, `timeout`, `full`, `internal`), the **component**
it came from, the **operation** it was doing, the **trace** it concerns, and
the cause. Components build them with constructors
(`fault.Unavailable(fault.Scorer, "score trace", err)`) and callers branch on
`fault.KindOf` and `fault.Retryable`, never on another package's sentinel
values or error strings.

Errors that are handled rather than returned (a retry, a dead letter, a
failed export) are **reported** to an `observe.Reporter`, which every
component is given. The engine's reporter is an `observe.Pipeline` built on
`log/slog`: each error becomes one record (its operation as the message, its
kind, component, trace ID and cause as attributes) handed to every sink
under `observability.error_sinks` through `slog.NewMultiHandler`. Sinks are
plain slog handlers: JSON lines on stdout or in a file, and OTLP logs to an
OpenTelemetry collector through the `otelslog` bridge, so the engine's own
errors land beside its metrics. A small wrapping handler counts each
warning and error for `tailwise.errors` and notes a failing sink on stderr
without stopping the engine.

- **Errors are classified once, then acted on.** The scorer classifies each
  failure (`classify`): a 400, 413 or 422 is `rejected`, anything else
  (timeouts, 5xx, connection errors, garbage) is `unavailable` or `timeout`.
  The pipeline defers anything but `rejected` and `invalid` to the queue.
- **One level of retry per path.** The scorer makes one call. A queued
  trace that fails with a system error is returned to the queue, which
  redelivers it with `queue.redelivery_backoff` and dead-letters it after
  `queue.max_deliver` deliveries. The synchronous endpoint has no queue, so
  the engine retries there instead, with the model's `retry.Policy`
  (`scorer.max_retries`, jittered `scorer.backoff`). Export sends make one
  request each; the export queue retries them. Retries never multiply.
- **Degrade rather than fail** where retrying cannot help: a rejected trace,
  or any failure on the synchronous endpoint, gets the policy's random
  fallback decision instead of an error.
- **HTTP status from the kind, in one place.** `server.writeFault` maps
  `full` and the retryable kinds to 503 with `Retry-After`, `invalid` to 400,
  and anything else to 500.

### Graceful shutdown in dependency order

`signal.NotifyContext` turns SIGINT and SIGTERM into a cancelled context.
`run` then stops things in the order that loses nothing: the HTTP server,
the OTLP receivers, the assemblers, the workers, then the export queues,
which have their own time budget so a slow drain cannot starve them, and
last the queue and the broker's connection.

### Golden files for the slice

`trace.BuildState` writes the text the model reads, and a change to it can
shift every score. `testdata/golden/` pins its exact output for each fixture
in `testdata/traces/`; `TestBuildStateGolden` fails on any difference, and
`make golden` rewrites the files after an intended change, for review in the
diff.

## Patterns not used, and why

| Pattern | Why not |
| --- | --- |
| Web framework and middleware chain | Six routes and no cross-cutting middleware. Limits and timeouts are set where they apply. |
| DI container | The object graph is small and fixed. |
| Repository layer or ORM | There is no database. |
| Plugin or strategy registry | One scorer, one policy, one exporter. An interface is enough to fake them in tests. |
| Hexagonal or clean-architecture layering | The package split already separates transport, pipeline and integrations; formal ports and adapters would add names without adding separation. |
| Global state | Two process-wide settings exist, both set once in `main`: the default `slog` logger and OpenTelemetry's error handler. |
| Hot config reload | A restart is cheap, and queued work survives it. |

## Concurrency model

| What | Goroutines |
| --- | --- |
| JSON API | one per request (`net/http`) |
| OTLP HTTP and gRPC | one per request or call |
| Queue workers | `server.workers`, each handling one trace at a time |
| Assemblers | `otlp.assemblers` |
| Export | per back end: `exporters[].concurrency` workers draining its queue (a JetStream consumer), each send one synchronous OTLP request |
| Embedded NATS | the NATS server's own goroutines, when `queue.url` is empty |

Components shared across goroutines (`scorer.Client`, `pipeline.Policy`,
`export.Dispatcher`, `otlpbackend.Backend`, `natsq.Queue`) are safe to
call concurrently. There are no locks in the pipeline itself.

## Testing approach

- **No network and no Docker.** `make test` runs `go vet` and
  `go test -race`.
- **Real NATS, in-process.** `internal/queue/queuetest` starts an actual
  NATS server on a loopback port, so redelivery, dead-lettering and stream
  limits are tested against JetStream itself.
- **Fakes at the interfaces.** The model is an `httptest.Server` returning the
  canned responses in `testdata/kev/`; a trace back end is a fake OTLP
  uploader or a fake `AbstractBackend`.
- **One suite for both connections.** `internal/queue/conformance_test.go` runs
  every queue scenario (delivery, depth, a full queue, redelivery, dead
  letters, stop timeouts, span assembly) against an external NATS server and
  the embedded one, and `internal/export/conformance_test.go` does the same for
  the export queues.
- **Specs kept in step by tests.** `TestOpenAPICoversRoutes` fails if a route
  is missing from `openapi.yaml`, and `TestSchemaMatchesConfig` fails if
  `tailwise.schema.yaml` and the config structs drift apart.

## Where to change things

| To change | Look in |
| --- | --- |
| A config key or default | `internal/config/config.go` (`Default`), `validate.go`, and `engine.go` if a component reads it; then `tailwise.schema.yaml` and `tailwise.yaml` |
| Where settings come from | anything that produces a `config.File`, then `config.Build` (`internal/config/load.go`) |
| An HTTP route | `internal/server/server.go`, then `internal/server/openapi.yaml` |
| What the model is shown | `internal/trace/slice.go`, then `make golden` and review the diff |
| How spans map to a trace | `internal/otlp/convert.go` |
| The keep or drop rule | `internal/pipeline/policy.go` |
| Retry and dead-letter behaviour | `internal/retry/retry.go` (the policy), `internal/pipeline/engine.go` (synchronous scoring), `internal/jsq/jsq.go` (both NATS queues), `internal/queue/natsq/queue.go` (traces), `internal/export/natsq/queue.go` (exports) |
| How the engine reaches NATS, or the embedded server | `internal/broker/broker.go` |
| The pipeline's steps or its interfaces | `internal/pipeline/engine.go`, `internal/pipeline/interfaces.go` |
| How a trace finds its model | `internal/pipeline/model.go` (routing), `internal/otlp/convert.go` (`Assembler.agentOf`) |
| A model server with another protocol | a new `pipeline.Scorer`, built in `internal/scorer/factory.go` |
| A metric | `internal/metrics/metrics.go`, and the hook interface in the package that records it |
| Where errors are written | `openSink` in `internal/observe/sinks.go`, and `observability.error_sinks` in the config |
| What the trace back ends receive | `internal/export/otlpbackend/backend.go` (attributes), `internal/otlp/convert.go` (`Annotate`, for forwarded spans) |
| A new kind of trace back end | a new `export.AbstractBackend` and its `export.BackendFunc` |
| Start-up and shutdown order | `cmd/tailwise/main.go` |
