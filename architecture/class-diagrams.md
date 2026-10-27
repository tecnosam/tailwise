# Class diagrams

The engine's structs and interfaces, grouped by area, with the fields and
methods that matter for how they fit together. Go has no classes: a box is a
struct or an interface, and implementing an interface is implicit, drawn
here as a dashed arrow. Types from other areas appear once more, without
members, where they connect. Where two packages use the same name (several
have a `Config`), the box is named `PackageName` (`ScorerConfig`,
`QueueConfig`, ...).

```
A ..|> B    A implements interface B
A *-- B     A contains B (a field holding it by value)
A o-- B     A holds B (a pointer, interface, slice or map field)
A ..> B     A uses B (takes or returns it)
```

- [1. The core: pipeline](#1-the-core-pipeline)
- [2. Scoring: scorer](#2-scoring-scorer)
- [3. The trace queue: queue, natsq, jsq, broker](#3-the-trace-queue-queue-natsq-jsq-broker)
- [4. Export: export, natsq, otlpbackend](#4-export-export-natsq-otlpbackend)
- [5. Intake: server and otlp](#5-intake-server-and-otlp)
- [6. Configuration: config](#6-configuration-config)
- [7. Errors, observability and metrics](#7-errors-observability-and-metrics)

## 1. The core: pipeline

`pipeline.Engine` runs a trace through route, slice, score, decide
and export. It knows its parts only by the interfaces it declares.

```mermaid
classDiagram
    direction LR
    namespace pipeline {
        class Engine {
            -deps Deps
            +Process(ctx, Trace) Outcome
            +ProcessQueued(ctx, Trace) Outcome, error
            +Ready(ctx) error
            -score(ctx, Trace, Model, queued) Score, error
            -scored(Trace, Model, Score) Record
            -fallback(...) Record
        }
        class Deps {
            +Models ModelRouter
            +Policy *Policy
            +Exporter Exporter
            +Reporter observe.Reporter
            +Metrics Metrics
            +Logger *slog.Logger
        }
        class Scorer {
            <<interface>>
            +Score(ctx, State) Score, error
            +Ready(ctx) error
        }
        class Exporter {
            <<interface>>
            +Export(ctx, Record) bool
        }
        class Publisher {
            <<interface>>
            +Publish(ctx, Trace) error
            +Ready() error
        }
        class Metrics {
            <<interface>>
            +ScoreFinished(ctx, ScoreResult)
            +TraceDecided(ctx, Record)
        }
        class ModelRouter {
            <<interface>>
            +Route(AgentName) Model
            +Ready(ctx) error
        }
        class AgentRouter {
            -fallback Model
            -byAgent map~AgentName, Model~
        }
        class Model {
            +Name ModelName
            +Scorer Scorer
            +Retry retry.Policy
            +Agent AgentLabel
        }
        class Policy {
            -cfg PolicyConfig
            -rand func() float64
            +Decide(pKeep) Decision
            +Fallback() Decision
        }
        class PolicyConfig {
            +KeepThreshold Probability
            +RandomFloor Probability
        }
        class Score {
            +P Probability
            +Model ModelName
            +Latency Duration
            +Cause string
            +Explanation string
        }
        class Decision {
            +Keep bool
            +Scored bool
            +PKeep Probability
            +Rule Rule
            +Reason string
            +Cause string
            +Label() string
        }
        class Record {
            +Trace trace.Trace
            +Agent AgentLabel
            +Decision Decision
            +Model ModelName
            +Latency Duration
        }
        class Outcome {
            +Record Record
            +Exported bool
            +Duration Duration
        }
    }
    namespace trace {
        class Trace {
            +TraceID string
            +Agent AgentName
            +Question string
            +ToolCalls []ToolCall
            +Evidence []string
            +FinalAnswer string
            +OTLP []byte
        }
        class ToolCall {
            +Name string
            +Args RawMessage
            +Status RawMessage
        }
    }

    Engine *-- Deps
    Deps o-- ModelRouter
    Deps o-- Policy
    Deps o-- Exporter
    Deps o-- Metrics
    AgentRouter ..|> ModelRouter
    AgentRouter *-- Model
    Model o-- Scorer
    Policy *-- PolicyConfig
    Engine ..> Score
    Policy ..> Decision
    Record *-- Decision
    Record *-- Trace
    Outcome *-- Record
    Trace *-- ToolCall

    scorer_Client ..|> Scorer
    export_Dispatcher ..|> Exporter
    natsq_Queue ..|> Publisher
    metrics_Recorder ..|> Metrics
```

`scorer_Client`, `export_Dispatcher`, `natsq_Queue` and `metrics_Recorder` are
`scorer.Client`, `export.Dispatcher`, `natsq.Queue` (the trace queue) and
`metrics.Recorder`, drawn in full below.

## 2. Scoring: scorer

`scorer.BuildRouter` turns the configured models into `pipeline.Model`s,
each with a `scorer.Client` speaking `/v1/systemone`.

```mermaid
classDiagram
    direction LR
    namespace scorer {
        class Client {
            -cfg ScorerConfig
            -probeURL string
            +Score(ctx, State) Score, error
            +Ready(ctx) error
            -attempt(ctx, body) Score, error
            -parse(raw, elapsed) Score, error
            -decisionAnswer(response) Probability, error
            -reasonAnswer(response) cause, explanation
        }
        class ScorerConfig {
            +Endpoint string
            +Model string
            +Questions RawMessage
            +DecisionQuestion string
            +ReasonQuestion string
            +Reasons map~string, string~
            +APIKey string
            +Timeout Duration
            +HTTPClient *http.Client
        }
        class ModelConfig {
            +Client ScorerConfig
            +Retry retry.Policy
        }
        class Models {
            +Default ModelConfig
            +ByAgent map~AgentName, ModelConfig~
        }
        class request {
            +Model string
            +State string
            +Questions RawMessage
        }
        class response {
            +Model string
            +Answers map~string, RawMessage~
            +LatencyMS *float64
            +EvaluationTimeMS *float64
        }
        class StatusError {
            +Code int
            +Body string
        }
    }
    class PipelineScorer["pipeline.Scorer"] {
        <<interface>>
    }
    class AgentRouter["pipeline.AgentRouter"]

    Client *-- ScorerConfig
    ModelConfig *-- ScorerConfig
    Models *-- ModelConfig
    Client ..> request : sends
    Client ..> response : reads
    Client ..> StatusError : non-2xx
    Client ..|> PipelineScorer
    Models ..> AgentRouter : BuildRouter(Models)
```

## 3. The trace queue: queue, natsq, jsq, broker

`queue` holds the contract; `natsq` implements it on the generic JetStream
work queue, `jsq.Queue[T]`, which the export queues share. `broker` owns the
one NATS connection, to an external server or an embedded one.

```mermaid
classDiagram
    direction TB
    namespace queue {
        class AbstractBroker {
            <<interface>>
            +Publish(ctx, Trace) error
            +PublishSpans(ctx, map~string, []byte~) error
            +StartWorkers(n, Handler) Stopper
            +StartAssemblers(n, AssembleFunc) Stopper
            +Ready() error
            +Depth(ctx) int64, error
            +Close()
        }
        class Stopper {
            <<interface>>
            +Stop(ctx) error
        }
        class QueueConfig {
            +Stream, Subject, Consumer string
            +MaxMessages int64
            +AckWait Duration
            +Redelivery retry.Policy
            +DLQStream, DLQSubject string
            +Spans SpanConfig
            +PollWait Duration
        }
        class SpanConfig {
            +Stream, Subject string
            +PendingStream, PendingSubject string
            +IdleTimeout, MaxWait, MaxAge Duration
        }
    }
    namespace natsq {
        class NatsQueue["Queue"] {
            -cfg QueueConfig
            -conn *Conn
            -work *jsq.Queue~Trace~
            -spans Stream
            -pendingCons Consumer
            +Publish(ctx, Trace) error
            +PublishSpans(ctx, map) error
            +StartWorkers(n, Handler) Stopper
            +StartAssemblers(n, AssembleFunc) Stopper
        }
        class Assemblers {
            -q *Queue
            -assemble AssembleFunc
            -handle(marker)
        }
        class assembly {
            -marker Msg
            -traceID string
            -run()
            -assembleAndQueue(lastSeq) Op, error
            -remarkLateSpans()
        }
    }
    namespace jsq {
        class WorkQueue["Queue~T~"] {
            -cfg JsqConfig
            -conn *Conn
            -cons Consumer
            -decode func([]byte) T, error
            +Publish(ctx, traceID, []byte) error
            +Start(n, Handler~T~) *Workers
            +Depth(ctx) int64, error
            +Flush(ctx) error
            -handle(pool, msg, h)
            -deadLetter(msg, Delivery, reason, cause)
        }
        class JsqConfig["Config"] {
            +Stream, Subject string
            +MaxMsgs int64
            +Consumer string
            +AckWait Duration
            +DLQStream, DLQSubject string
            +Retry retry.Policy
            +Retryable func(error) bool
            +Component, PublishOp, RetryOp, DeadLetterOp
        }
        class Delivery {
            +TraceID string
            +Attempt int
            +Sequence uint64
        }
        class Workers
    }
    namespace common {
        class Conn {
            +NC *nats.Conn
            +JS JetStream
            +Replicas int
            +EnsureStreams(ctx, ...StreamConfig) error
            +Ready() error
        }
        class Pool {
            +Start(n, run)
            +Stop(ctx) error
            +Stopping() bool
            +WorkCtx() Context
        }
    }
    namespace broker {
        class Broker {
            -conn *Conn
            -srv *server.Server
            -tempDir string
            +Ingest(ctx, QueueConfig) AbstractBroker, error
            +ExportQueues(Streams) QueueFunc
            +Kind() Kind
            +Ephemeral() bool
            +Close()
        }
        class BrokerConfig["Config"] {
            +URL string
            +StoreDir string
            +Replicas int
            +Auth NATSAuth
            +TLS NATSTLS
        }
    }

    NatsQueue ..|> AbstractBroker
    NatsQueue *-- QueueConfig
    QueueConfig *-- SpanConfig
    NatsQueue o-- WorkQueue
    NatsQueue o-- Conn
    NatsQueue ..> Assemblers : StartAssemblers
    Assemblers ..|> Stopper
    Assemblers --|> Pool : embeds
    Assemblers ..> assembly : one per marker
    WorkQueue *-- JsqConfig
    WorkQueue o-- Conn
    WorkQueue ..> Delivery : to the handler
    WorkQueue ..> Workers : Start
    Workers --|> Pool : embeds
    Workers ..|> Stopper
    Broker o-- Conn
    Broker ..> NatsQueue : Ingest
    Broker ..> BrokerConfig : New(Config)
```

## 4. Export: export, natsq, otlpbackend

`export.Dispatcher` gives each back end its own queue. The back ends and
their queues are built by plain functions (`BackendFunc`, `QueueFunc`).

```mermaid
classDiagram
    direction TB
    namespace export {
        class Dispatcher {
            -routes []route
            -reporter Reporter
            +Export(ctx, Record) bool
            +Flush(ctx) error
            +Shutdown(ctx) error
            +Depths() []QueueDepth
        }
        class route {
            -name string
            -backend AbstractBackend
            -queue AbstractQueue
        }
        class DispatcherConfig {
            +Backends []BackendConfig
            +NewBackend BackendFunc
            +NewQueue QueueFunc
            +Reporter Reporter
            +Metrics SendMetrics
        }
        class AbstractBackend {
            <<interface>>
            +Send(ctx, Record) error
            +Shutdown(ctx) error
        }
        class AbstractQueue {
            <<interface>>
            +Enqueue(ctx, Record) error
            +Flush(ctx) error
            +Shutdown(ctx) error
            +Depth(ctx) int64, error
        }
        class BackendConfig {
            +Name, Endpoint string
            +Headers, ResourceAttributes map
            +QueueSize, Concurrency int
            +Retry retry.Policy
        }
        class ExportQueueConfig["QueueConfig"] {
            +Name string
            +Size, Concurrency int
            +Retry retry.Policy
            +Send SendFunc
            +Reporter Reporter
        }
        class BackendFunc {
            <<func>>
            +call(ctx, BackendConfig) AbstractBackend, error
        }
        class QueueFunc {
            <<func>>
            +call(ctx, QueueConfig) AbstractQueue, error
        }
    }
    namespace exportnatsq {
        class natsQueue {
            -work *jsq.Queue~Record~
            -workers *jsq.Workers
            -maxPayload int
        }
        class Streams {
            +Stream, Subject, Consumer string
            +DLQStream, DLQSubject string
            +DLQMaxAge, PollWait Duration
        }
        class recordMessage {
            +Trace Trace
            +Agent, Rule, Reason, Cause, Model string
            +Keep, Scored bool
            +PKeep float64
            +LatencyNS int64
        }
    }
    namespace otlpbackend {
        class Backend {
            -client Uploader
            -resource []KeyValue
            +Send(ctx, Record) error
            +Shutdown(ctx) error
            -rebuild(Trace) []ResourceSpans
        }
        class Uploader {
            <<interface>>
            +UploadTraces(ctx, []ResourceSpans) error
        }
    }
    class PipelineExporter["pipeline.Exporter"] {
        <<interface>>
    }

    Dispatcher ..|> PipelineExporter
    Dispatcher *-- route
    route o-- AbstractBackend
    route o-- AbstractQueue
    DispatcherConfig *-- BackendConfig
    DispatcherConfig o-- BackendFunc
    DispatcherConfig o-- QueueFunc
    Dispatcher ..> DispatcherConfig : NewDispatcher
    QueueFunc ..> ExportQueueConfig
    natsQueue ..|> AbstractQueue
    natsQueue ..> recordMessage : stores records as
    QueueFunc ..> natsQueue : natsq.Queues(conn, Streams)
    QueueFunc ..> Streams
    Backend ..|> AbstractBackend
    Backend o-- Uploader
    BackendFunc ..> Backend : otlpbackend.Build
```

## 5. Intake: server and otlp

The JSON API and the OTLP receivers both end at the trace queue; `otlp` also
turns stored spans into a `trace.Trace` and annotates them for export.

```mermaid
classDiagram
    direction LR
    namespace server {
        class Server {
            -opts Options
            -mux *ServeMux
            +Handler() http.Handler
            -handleEnqueue(w, r)
            -handleScore(w, r)
            -readTrace(w, r) Trace, bool
        }
        class Options {
            +Engine *pipeline.Engine
            +Queue pipeline.Publisher
            +Reporter Reporter
            +Metrics RequestMetrics
            +MaxBodyBytes int64
        }
        class Result {
            +TraceID, Agent, Decision string
            +PKeep *float64
            +Rule, Reason, Cause string
            +Model string
            +LatencyMS float64
            +Exported bool
        }
    }
    namespace otlp {
        class Receiver {
            -sink Sink
            -grpcServer *grpc.Server
            -httpServer *http.Server
            +Start(grpcAddr, httpAddr) error
            +Export(ctx, ExportTraceServiceRequest) Response, error
            +Shutdown(ctx) error
            -accept(ctx, request) error
        }
        class Sink {
            <<interface>>
            +PublishSpans(ctx, map~string, []byte~) error
        }
        class Assembler {
            +AgentAttributes []string
            +Assemble(traceID, [][]byte) Trace, error
        }
        class Annotation {
            +Resource []KeyValue
            +Root []KeyValue
        }
        class otlpFuncs["functions"] {
            <<utility>>
            +Split([]ResourceSpans) map, skipped, error
            +Annotate([]byte, Annotation) []ResourceSpans, error
            +AnnotateSpans([]ResourceSpans, Annotation) []ResourceSpans, error
            +SpansFromTrace(traceID, Trace) []Span
        }
    }
    class PipelineEngine["pipeline.Engine"]
    class PipelinePublisher["pipeline.Publisher"] {
        <<interface>>
    }
    class TraceQueue["natsq.Queue"]

    Server *-- Options
    Options o-- PipelineEngine
    Options o-- PipelinePublisher
    Server ..> Result : sync response
    Receiver o-- Sink
    Receiver ..> otlpFuncs : Split
    TraceQueue ..|> Sink
    TraceQueue ..|> PipelinePublisher
    Assembler ..> otlpFuncs
    otlpFuncs ..> Annotation
```

## 6. Configuration: config

`config.File` is the YAML file as written; `config.Build` turns it into
`config.Engine`, every component's settings in that component's own type.

```mermaid
classDiagram
    direction LR
    namespace config {
        class File {
            +Server Server
            +Queue Queue
            +OTLP OTLP
            +Scorer Scorer
            +Agents Agents
            +Policy Policy
            +Exporters []Exporter
            +Observability Observability
        }
        class Scorer {
            +Kind, Endpoint, Model string
            +APIKeyEnv string
            +Manifest string
            +DecisionQuestion, ReasonQuestion string
            +Timeout Duration
            +MaxRetries int
            +Backoff []Duration
        }
        class Agents {
            +Attributes []string
            +Models map~string, Scorer~
        }
        class Exporter {
            +Name, Endpoint string
            +Headers map
            +HeadersEnv string
            +QueueSize, Concurrency, MaxDeliver int
            +Backoff []Duration
        }
        class Manifest {
            +Model string
            +Questions RawMessage
            +Reasons map~string, string~
        }
        class ConfigEngine["Engine"] {
            +Server ServerSettings
            +OTLP OTLPSettings
            +Broker broker.Config
            +Queue queue.Config
            +ExportStreams natsq.Streams
            +Exporters []export.BackendConfig
            +Models scorer.Models
            +Policy pipeline.PolicyConfig
            +Metrics metrics.Config
            +ErrorSinks []observe.SinkConfig
            +Summary []any
            +Warnings []string
        }
        class secrets {
            -apiKeys map
            -natsPassword, natsToken string
            -exporterHeaders, sinkHeaders []map
            -metricsHeaders map
        }
        class loadFuncs["functions"] {
            <<utility>>
            +Load(path) *Engine, error
            +Read(path) File, error
            +Build(File, dir, getenv) *Engine, error
            +Default() File
        }
    }

    File *-- Scorer
    File *-- Agents
    Agents o-- Scorer
    File *-- Exporter
    loadFuncs ..> File : Read, Default
    loadFuncs ..> secrets : read from the environment
    loadFuncs ..> Manifest : file, or the built-in one
    loadFuncs ..> ConfigEngine : Build
```

## 7. Errors, observability and metrics

Every failure is a `fault.Error`. Components report it to an
`observe.Reporter`; `metrics.Recorder` implements every metrics hook the other
packages declare.

```mermaid
classDiagram
    direction LR
    namespace fault {
        class Error {
            +Kind Kind
            +Component Component
            +Op Op
            +TraceID string
            +Severe bool
            +Attrs map~string, any~
            +Cause error
            +WithTrace(id) *Error
            +With(key, value) *Error
            +AsSevere() *Error
        }
        class Kind {
            <<enumeration>>
            internal
            invalid
            rejected
            unavailable
            timeout
            full
        }
    }
    namespace observe {
        class Reporter {
            <<interface>>
            +Report(ctx, error)
        }
        class ErrorCounter {
            <<interface>>
            +ErrorReported(ctx, kind, component)
        }
        class ObservePipeline["Pipeline"] {
            -logger *slog.Logger
            +Report(ctx, error)
            +Close(ctx) error
        }
        class counting {
            -counter ErrorCounter
            +Handle(ctx, slog.Record) error
        }
        class SinkConfig {
            +Kind SinkKind
            +Path, Endpoint string
            +Headers map
        }
    }
    namespace metrics {
        class Recorder {
            +RequestFinished(ctx, Request)
            +ScoreFinished(ctx, ScoreResult)
            +TraceDecided(ctx, Record)
            +SendFinished(ctx, Send)
            +ErrorReported(ctx, kind, component)
            +ObserveQueues(...QueueDepth) error
        }
        class MetricsPipeline["Pipeline"] {
            -shutdown func
            +Shutdown(ctx) error
        }
    }
    namespace retry {
        class Policy {
            +Attempts int
            +Backoff []Duration
            +Jitter bool
            +Exhausted(attempts) bool
            +Delay(attempts) Duration
        }
    }
    class PipelineMetrics["pipeline.Metrics"] {
        <<interface>>
    }
    class RequestMetrics["common.RequestMetrics"] {
        <<interface>>
    }
    class SendMetrics["export.SendMetrics"] {
        <<interface>>
    }

    Error *-- Kind
    ObservePipeline ..|> Reporter
    ObservePipeline ..> counting : wraps its slog handler
    counting o-- ErrorCounter
    ObservePipeline ..> SinkConfig : observe.Open
    ObservePipeline ..> Error : turns into a log record
    MetricsPipeline --|> Recorder : embeds
    Recorder ..|> ErrorCounter
    Recorder ..|> PipelineMetrics
    Recorder ..|> RequestMetrics
    Recorder ..|> SendMetrics
```

`retry.Policy` is the one retry policy everything shares: `queue.Config.Redelivery`
for the trace queue, `export.QueueConfig.Retry` for each back end, and
`pipeline.Model.Retry` for the synchronous endpoint.
