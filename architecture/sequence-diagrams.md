# Sequence diagrams

How one trace moves through the engine, package by package, and what is
passed at each step. The README has the high-level picture; these follow the
calls. Participants are named `package.Type`. Arrows carry the function
called and the value passed or returned.

- [The values passed along](#the-values-passed-along)
- [1. Queued intake: POST /v1/traces](#1-queued-intake-post-v1traces)
- [2. A worker scores a queued trace](#2-a-worker-scores-a-queued-trace)
- [3. An export worker sends a kept trace](#3-an-export-worker-sends-a-kept-trace)
- [4. OTLP intake: spans over gRPC or HTTP](#4-otlp-intake-spans-over-grpc-or-http)
- [5. An assembler turns spans into a trace](#5-an-assembler-turns-spans-into-a-trace)
- [6. Synchronous scoring: POST /v1/traces:score](#6-synchronous-scoring-post-v1tracesscore)
- [7. Start-up: from the config to running components](#7-start-up-from-the-config-to-running-components)

## The values passed along

| Value | Package | What it is | Where it travels |
| --- | --- | --- | --- |
| request body (JSON) | | The client's trace | HTTP into `server.Server` |
| `ExportTraceServiceRequest` | OTLP protobuf | A batch of spans | gRPC or HTTP into `otlp.Receiver` |
| `map[string][]byte` | | Trace ID to a serialized `TracesData` of that trace's spans | `otlp.Split` to the span store |
| `trace.Trace` | `internal/trace` | The agent run: question, tool calls, evidence, final answer, and the original spans in `OTLP` | Intake, the trace queue (as JSON), the engine |
| `trace.State` | `internal/trace` | The trace rendered as the text the model reads | `pipeline.Engine` to `pipeline.Scorer` |
| `request` / `response` | `internal/scorer` | The `/v1/systemone` JSON: model, state, questions; answers per question | `scorer.Client` to and from Jev |
| `pipeline.Score` | `internal/pipeline` | The probability the answer is wrong, model, latency, the chosen cause and its sentence | `scorer.Client` to `pipeline.Engine` |
| `pipeline.Decision` | `internal/pipeline` | Keep or drop, the probability, rule, reason, cause | `pipeline.Policy` and `pipeline.Engine` |
| `pipeline.Record` | `internal/pipeline` | The trace with its decision, agent label, model and latency | The engine to the dispatcher, the export queues (as `recordMessage` JSON), the back ends |
| `pipeline.Outcome` | `internal/pipeline` | The record, whether it was queued for export, and the run's duration | The engine to its caller |
| `jsq.Delivery` | `internal/jsq` | One message as a worker got it: trace ID, attempt, stream sequence | The work queue to its handler |
| `[]*tracepb.ResourceSpans` | OTLP protobuf | Spans annotated with the `eval.*` decision | `otlpbackend.Backend` to the back end |
| `fault.Error` | `internal/fault` | Any failure: kind, component, operation, trace ID, attributes | Returned everywhere; reported to `observe.Reporter` |

## 1. Queued intake: POST /v1/traces

The 202 is sent only after JetStream has stored the trace.

```mermaid
sequenceDiagram
    autonumber
    actor Client as Agent or client
    participant Srv as server.Server
    participant Tr as trace
    participant Q as natsq.Queue
    participant WQ as jsq.Queue[trace.Trace]
    participant JS as NATS JetStream

    Client->>Srv: POST /v1/traces (JSON body)
    Srv->>Tr: Decode(body)
    Tr-->>Srv: trace.Trace (validated)
    Note over Srv: readTrace: TraceID = trace.NewID() if empty,<br/>OTLP = nil
    Srv->>Q: Publish(ctx, trace.Trace)
    Q->>Q: json.Marshal(trace.Trace)
    Q->>WQ: Publish(ctx, traceID, []byte)
    WQ->>JS: PublishMsg(tailwise.traces, header Tailwise-Trace-Id)
    alt stored
        JS-->>WQ: PubAck
        WQ-->>Q: nil
        Q-->>Srv: nil
        Srv-->>Client: 202 {"trace_id"}
    else stream at max_messages, or NATS down
        JS-->>WQ: error
        WQ-->>Srv: fault.Error (KindFull or KindUnavailable)
        Srv-->>Client: 503 + Retry-After
    end
```

## 2. A worker scores a queued trace

Each of `server.workers` goroutines pulls one message at a time from the
shared durable consumer. The model is called once per delivery; the queue
does the retrying.

```mermaid
sequenceDiagram
    autonumber
    participant JS as NATS JetStream
    participant WQ as jsq.Queue[trace.Trace]
    participant Q as natsq.Queue
    participant Eng as pipeline.Engine
    participant R as pipeline.AgentRouter
    participant Tr as trace
    participant Sc as scorer.Client
    participant Jev as Jev (/v1/systemone)
    participant Pol as pipeline.Policy
    participant D as export.Dispatcher
    participant EQ as export natsq queue

    JS->>WQ: Next() → jetstream.Msg
    WQ->>WQ: decode: trace.Decode(msg.Data()) → trace.Trace
    WQ->>Q: handler(ctx, trace.Trace, *jsq.Delivery)
    Note over Q: no TraceID: queue.SequenceID(stream, d.Sequence)
    Q->>Eng: ProcessQueued(ctx, trace.Trace)
    Eng->>R: Route(trace.Agent)
    R-->>Eng: pipeline.Model (Scorer, Retry, AgentLabel)
    Eng->>Tr: BuildState(trace.Trace)
    Tr-->>Eng: trace.State
    Eng->>Sc: Score(ctx, trace.State)
    Sc->>Jev: POST request{model, state, questions}
    Jev-->>Sc: response{answers: answer_wrong noul, why_wrong choice}
    Note over Sc: decisionAnswer → P<br/>reasonAnswer → Cause, Explanation
    Sc-->>Eng: pipeline.Score, or fault.Error
    alt scored
        Eng->>Pol: Decide(Score.P)
        Pol-->>Eng: pipeline.Decision (Keep, PKeep, Rule)
        Note over Eng: Reason, Cause = Score.Explanation, Score.Cause
        Eng->>D: Export(ctx, pipeline.Record)
        opt Decision.Keep
            loop each back end
                D->>EQ: Enqueue(ctx, pipeline.Record)
                EQ->>JS: PublishMsg(tailwise.exports.<name>, recordMessage JSON)
            end
        end
        D-->>Eng: exported bool
        Note over Eng: metrics.TraceDecided, one "decision" log line
        Eng-->>Q: pipeline.Outcome, nil
        Q-->>WQ: nil
        WQ->>JS: DoubleAck
    else system error (down, timeout, 5xx, 401, 429)
        Eng-->>WQ: fault.Error
        alt attempts left
            WQ->>JS: NakWithDelay(queue.redelivery_backoff)
        else queue.max_deliver reached
            WQ->>JS: PublishMsg(tailwise.traces.dlq, same body + Tailwise-Dead-* headers)
            WQ->>JS: DoubleAck
        end
    else model rejects the trace (400, 413, 422)
        Eng->>Pol: Fallback()
        Pol-->>Eng: pipeline.Decision (Rule fallback, random keep)
        Note over Eng: exported as above; the queue acknowledges
    end
```

## 3. An export worker sends a kept trace

Each back end has its own queue and `exporters[].concurrency` workers. A
send is one OTLP/HTTP request; the export queue retries it.

```mermaid
sequenceDiagram
    autonumber
    participant JS as NATS JetStream
    participant EW as jsq.Queue[pipeline.Record]
    participant T as export.timed
    participant B as otlpbackend.Backend
    participant O as otlp
    participant Up as otlptracehttp client
    participant BE as Back end (Phoenix, Langfuse, ...)

    JS->>EW: Next() → jetstream.Msg
    EW->>EW: decodeRecord(msg.Data()) → pipeline.Record
    EW->>T: Send(ctx, pipeline.Record)
    T->>B: Send(ctx, pipeline.Record)
    Note over B: evalAttrs(Record) → eval.decision, rule, reason,<br/>cause, model, p_keep, latency
    alt Record.Trace.OTLP set (came in as spans)
        B->>O: Annotate(OTLP bytes, Annotation)
    else from the JSON API
        B->>B: rebuild(trace.Trace): SpansFromTrace, random span IDs, now
        B->>O: AnnotateSpans([]*ResourceSpans, Annotation)
    end
    O-->>B: []*tracepb.ResourceSpans
    B->>Up: UploadTraces(ctx, spans)
    Up->>BE: POST /v1/traces (protobuf)
    BE-->>Up: 200, or an error
    Up-->>B: error or nil
    B-->>T: nil, or fault.Error (KindUnavailable)
    Note over T: metrics.SendFinished(exporter, duration, err)
    T-->>EW: error or nil
    alt sent
        EW->>JS: DoubleAck
    else retryable, attempts left
        EW->>JS: NakWithDelay(exporters[].backoff)
    else given up (max_deliver, or not retryable)
        EW->>JS: PublishMsg(tailwise.exports_dlq, body + Tailwise-Exporter, Tailwise-Dead-*)
        EW->>JS: DoubleAck
    end
```

## 4. OTLP intake: spans over gRPC or HTTP

The exporter gets a success only once every span batch is stored.

```mermaid
sequenceDiagram
    autonumber
    actor Agent as Instrumented agent
    participant Rcv as otlp.Receiver
    participant O as otlp
    participant Q as natsq.Queue (otlp.Sink)
    participant JS as NATS JetStream

    alt gRPC :4317
        Agent->>Rcv: Export(ExportTraceServiceRequest)
    else HTTP :4318
        Agent->>Rcv: POST /v1/traces (protobuf or JSON, maybe gzip)
        Note over Rcv: readBody, then proto.Unmarshal or<br/>unmarshalJSON (hex IDs to base64)
    end
    Rcv->>O: Split([]*ResourceSpans)
    O-->>Rcv: map[traceID][]byte (TracesData per trace), skipped count
    Rcv->>Q: PublishSpans(ctx, map[traceID][]byte)
    loop each trace
        Q->>JS: PublishMsgAsync(tailwise.spans.<trace id>, batch)
    end
    Q->>Q: waitAcks
    loop each trace, after its spans are stored
        Q->>JS: PublishMsgAsync(tailwise.pending.<trace id>, marker)
        Note over JS: one marker per trace:<br/>a second one is refused, which counts as success
    end
    alt stored
        Q-->>Rcv: nil
        Rcv-->>Agent: success (ExportTraceServiceResponse)
    else store unavailable
        Q-->>Rcv: fault.Error
        Rcv-->>Agent: gRPC Unavailable, or HTTP 503 + Retry-After
    end
```

## 5. An assembler turns spans into a trace

A marker stays unacknowledged while its trace is still receiving spans, then
the trace joins the queue of diagram 2.

```mermaid
sequenceDiagram
    autonumber
    participant JS as NATS JetStream
    participant As as natsq.Assemblers
    participant J as natsq.assembly
    participant A as otlp.Assembler
    participant WQ as jsq.Queue[trace.Trace]

    JS->>As: Next() on the pending consumer → marker
    As->>J: run()
    J->>JS: GetLastMsgForSubject(tailwise.spans.<trace id>)
    JS-->>J: last span batch (time, sequence)
    alt quiet for less than otlp.idle_timeout, and under otlp.max_wait
        J->>JS: NakWithDelay(time until complete)
    else complete
        J->>JS: GetMsg(seq...) for each batch up to the last
        JS-->>J: [][]byte
        J->>A: Assemble(traceID, [][]byte)
        Note over A: merge (drop repeated spans), findRoot,<br/>question and answer, toolCalls, evidence,<br/>agent from agents.attributes
        A-->>J: trace.Trace (OTLP = the merged TracesData), or ErrNotScorable
        opt scorable
            J->>WQ: Publish(traceID, json, WithMsgID(traceID:lastSeq))
            WQ->>JS: PublishMsg(tailwise.traces)
        end
        J->>JS: Purge(subject, up to lastSeq)
        J->>JS: DoubleAck(marker)
        opt spans arrived meanwhile
            J->>JS: Publish(tailwise.pending.<trace id>)
        end
    end
```

## 6. Synchronous scoring: POST /v1/traces:score

No trace queue: the engine scores in the request and retries a system error
itself. A kept trace still goes through the export queues of diagram 3.

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant Srv as server.Server
    participant Eng as pipeline.Engine
    participant Sc as scorer.Client
    participant Pol as pipeline.Policy
    participant D as export.Dispatcher

    Client->>Srv: POST /v1/traces:score (JSON body)
    Srv->>Srv: readTrace → trace.Trace
    Srv->>Eng: Process(ctx, trace.Trace)
    loop until scored, rejected, or Model.Retry runs out
        Eng->>Sc: Score(ctx, trace.State)
        Sc-->>Eng: pipeline.Score, or fault.Error
        opt retryable error
            Eng->>Eng: retry.Sleep(Model.Retry.Delay, jittered)
        end
    end
    alt scored
        Eng->>Pol: Decide(Score.P) → pipeline.Decision
    else not scored
        Eng->>Pol: Fallback() → pipeline.Decision
    end
    Eng->>D: Export(ctx, pipeline.Record)
    D-->>Eng: exported bool (queued, not yet sent)
    Eng-->>Srv: pipeline.Outcome
    Srv->>Srv: resultOf(Outcome) → server.Result
    Srv-->>Client: 200 Result{decision, p_keep, rule, reason, cause, ...}
```

## 7. Start-up: from the config to running components

`config` turns the file into each component's own settings; `main` wires
them, then serves.

```mermaid
sequenceDiagram
    autonumber
    participant M as main (app)
    participant C as config
    participant Br as broker.Broker
    participant Q as natsq.Queue
    participant D as export.Dispatcher
    participant S as scorer
    participant Rcv as otlp.Receiver

    M->>C: Load(path, or "" for the built-in defaults)
    C->>C: Read(path) → config.File
    C->>C: Build(File, dir, os.Getenv): env overrides, paths,<br/>validate, secrets, manifests
    C-->>M: *config.Engine (+ Warnings)
    M->>M: metrics.Start(Engine.Metrics), observe.Open(Engine.ErrorSinks)
    M->>Br: New(Engine.Broker)
    Note over Br: queue.url set: dial it<br/>empty: start an embedded NATS server
    M->>Br: Ingest(ctx, Engine.Queue)
    Br->>Q: natsq.New: trace stream, DLQ, consumer, span streams
    M->>D: NewDispatcher(Engine.Exporters, otlpbackend.Build, broker.ExportQueues)
    M->>S: BuildRouter(Engine.Models) → *pipeline.AgentRouter
    M->>M: pipeline.NewEngine(Deps), queue.StartWorkers(n, ProcessQueued)
    M->>Rcv: NewReceiver(Sink: queue), Start(:4317, :4318)
    M->>Q: StartAssemblers(n, otlp.Assembler.Assemble)
    M->>M: server.New, log "tailwise starting" + warnings, probe the model
    M->>M: serve until SIGINT or SIGTERM, then shutdown in order
```
