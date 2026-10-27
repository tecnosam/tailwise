# Package dependency graph

Which of the engine's packages imports which. Both graphs come from
`go list` (test-only packages left out); regenerate them with the commands at
the end when imports change.

- [The layers](#the-layers): every import, minus those already implied by
  another path, which shows the structure
- [Every import](#every-import): all 85 edges
- [What the graph guarantees](#what-the-graph-guarantees)
- [External libraries](#external-libraries)

## The layers

An arrow means "imports". An import that also follows from a longer path is
left out here: `config` imports `pipeline` directly, but also through
`scorer`, so only `config → scorer → pipeline` is drawn.

```mermaid
flowchart TB
    subgraph cmd["cmd: the binary"]
        cmd_tailwise["cmd/tailwise<br/>main: wires and runs everything"]
    end

    subgraph wiring["settings and wiring"]
        internal_config["internal/config<br/>File → Engine"]
        internal_broker["internal/broker<br/>JetStream connection, embedded NATS"]
    end

    subgraph adapters["adapters"]
        internal_server["internal/server<br/>JSON API"]
        internal_otlp["internal/otlp<br/>OTLP receivers, spans ↔ trace"]
        internal_scorer["internal/scorer<br/>System One client"]
        internal_export_otlpbackend["internal/export/otlpbackend<br/>OTLP/HTTP back end"]
        internal_export_natsq["internal/export/natsq<br/>export queues"]
        internal_queue_natsq["internal/queue/natsq<br/>trace queue, span assembly"]
    end

    subgraph contracts["contracts"]
        internal_export["internal/export<br/>Dispatcher, back-end and queue contracts"]
        internal_queue["internal/queue<br/>trace-queue contract and settings"]
    end

    subgraph core["core"]
        internal_metrics["internal/metrics<br/>the engine's own metrics"]
        internal_pipeline["internal/pipeline<br/>Engine, Policy, domain types"]
    end

    subgraph shared["shared"]
        internal_jsq["internal/jsq<br/>generic JetStream work queue"]
        internal_common["internal/common<br/>NATS conn, pull loop, pool"]
        internal_observe["internal/observe<br/>error reporting"]
        internal_retry["internal/retry"]
        internal_trace["internal/trace<br/>Trace, State"]
        internal_fault["internal/fault<br/>error type"]
    end

    cmd_tailwise --> internal_server
    cmd_tailwise --> internal_config
    cmd_tailwise --> internal_export_otlpbackend
    internal_config --> internal_broker
    internal_config --> internal_scorer
    internal_broker --> internal_export_natsq
    internal_broker --> internal_queue_natsq
    internal_export_natsq --> internal_jsq
    internal_export_natsq --> internal_export
    internal_export_otlpbackend --> internal_otlp
    internal_export_otlpbackend --> internal_export
    internal_queue_natsq --> internal_queue
    internal_queue --> internal_jsq
    internal_scorer --> internal_pipeline
    internal_export --> internal_metrics
    internal_metrics --> internal_pipeline
    internal_server --> internal_common
    internal_otlp --> internal_common
    internal_jsq --> internal_common
    internal_common --> internal_metrics
    internal_pipeline --> internal_observe
    internal_pipeline --> internal_retry
    internal_pipeline --> internal_trace
    internal_observe --> internal_fault
```

## Every import

```mermaid
flowchart LR
    cmd_tailwise[cmd/tailwise]
    internal_broker[internal/broker]
    internal_common[internal/common]
    internal_config[internal/config]
    internal_export[internal/export]
    internal_export_natsq[internal/export/natsq]
    internal_export_otlpbackend[internal/export/otlpbackend]
    internal_fault[internal/fault]
    internal_jsq[internal/jsq]
    internal_metrics[internal/metrics]
    internal_observe[internal/observe]
    internal_otlp[internal/otlp]
    internal_pipeline[internal/pipeline]
    internal_queue[internal/queue]
    internal_queue_natsq[internal/queue/natsq]
    internal_retry[internal/retry]
    internal_scorer[internal/scorer]
    internal_server[internal/server]
    internal_trace[internal/trace]

    cmd_tailwise --> internal_broker
    cmd_tailwise --> internal_config
    cmd_tailwise --> internal_export
    cmd_tailwise --> internal_export_otlpbackend
    cmd_tailwise --> internal_fault
    cmd_tailwise --> internal_metrics
    cmd_tailwise --> internal_observe
    cmd_tailwise --> internal_otlp
    cmd_tailwise --> internal_pipeline
    cmd_tailwise --> internal_queue
    cmd_tailwise --> internal_scorer
    cmd_tailwise --> internal_server
    cmd_tailwise --> internal_trace
    internal_broker --> internal_common
    internal_broker --> internal_export
    internal_broker --> internal_export_natsq
    internal_broker --> internal_observe
    internal_broker --> internal_queue
    internal_broker --> internal_queue_natsq
    internal_common --> internal_fault
    internal_common --> internal_metrics
    internal_common --> internal_observe
    internal_config --> internal_broker
    internal_config --> internal_export
    internal_config --> internal_export_natsq
    internal_config --> internal_metrics
    internal_config --> internal_observe
    internal_config --> internal_pipeline
    internal_config --> internal_queue
    internal_config --> internal_retry
    internal_config --> internal_scorer
    internal_config --> internal_trace
    internal_export --> internal_fault
    internal_export --> internal_metrics
    internal_export --> internal_observe
    internal_export --> internal_pipeline
    internal_export --> internal_retry
    internal_export_natsq --> internal_common
    internal_export_natsq --> internal_export
    internal_export_natsq --> internal_fault
    internal_export_natsq --> internal_jsq
    internal_export_natsq --> internal_pipeline
    internal_export_natsq --> internal_trace
    internal_export_otlpbackend --> internal_export
    internal_export_otlpbackend --> internal_fault
    internal_export_otlpbackend --> internal_observe
    internal_export_otlpbackend --> internal_otlp
    internal_export_otlpbackend --> internal_pipeline
    internal_export_otlpbackend --> internal_trace
    internal_jsq --> internal_common
    internal_jsq --> internal_fault
    internal_jsq --> internal_observe
    internal_jsq --> internal_retry
    internal_metrics --> internal_fault
    internal_metrics --> internal_pipeline
    internal_observe --> internal_fault
    internal_otlp --> internal_common
    internal_otlp --> internal_fault
    internal_otlp --> internal_metrics
    internal_otlp --> internal_observe
    internal_otlp --> internal_trace
    internal_pipeline --> internal_fault
    internal_pipeline --> internal_observe
    internal_pipeline --> internal_retry
    internal_pipeline --> internal_trace
    internal_queue --> internal_jsq
    internal_queue --> internal_observe
    internal_queue --> internal_retry
    internal_queue --> internal_trace
    internal_queue_natsq --> internal_common
    internal_queue_natsq --> internal_fault
    internal_queue_natsq --> internal_jsq
    internal_queue_natsq --> internal_observe
    internal_queue_natsq --> internal_queue
    internal_queue_natsq --> internal_trace
    internal_scorer --> internal_fault
    internal_scorer --> internal_pipeline
    internal_scorer --> internal_retry
    internal_scorer --> internal_trace
    internal_server --> internal_common
    internal_server --> internal_fault
    internal_server --> internal_metrics
    internal_server --> internal_observe
    internal_server --> internal_pipeline
    internal_server --> internal_trace
```

## What the graph guarantees

- **The core imports no adapter.** `pipeline` depends only on `trace`,
  `retry`, `observe` and `fault`. It declares what it needs (`Scorer`,
  `Exporter`, `Publisher`, `Metrics`, `ModelRouter`), and the adapters
  implement those interfaces. A new model server or back end never touches it.
- **Leaves.** `fault`, `retry` and `trace` import nothing of the engine's, so
  any package can use them.
- **Only `main` and `config` see the whole engine.** `config` imports the
  component packages to build their settings (`queue.Config`,
  `broker.Config`, `scorer.Models`, ...), and no component imports `config`.
  `main` wires the components together.
- **NATS stays behind the queue packages.** `internal/jsq`, `internal/common`
  and the two `natsq` packages are the only ones that speak JetStream;
  `broker` chooses between an external server and the embedded one.
- **No cycles.** Go forbids them; the graph is a DAG.

## External libraries

| Package | Uses |
| --- | --- |
| `internal/common`, `internal/jsq`, `internal/queue/natsq` | `nats.go` (the `jetstream` client) |
| `internal/broker` | `nats.go`, `nats-server` (the embedded server) |
| `internal/otlp` | `grpc`, `protobuf`, the OTLP protobuf types, `genproto` (gRPC status) |
| `internal/export/otlpbackend` | the OpenTelemetry OTLP/HTTP trace client and protobuf types |
| `internal/metrics` | the OpenTelemetry metrics SDK and its OTLP/HTTP exporter |
| `internal/observe` | the OpenTelemetry logs SDK, its OTLP/HTTP exporter, the `otelslog` bridge |
| `internal/config` | `yaml.v3` |

Every other package uses the standard library only.

## Regenerating

```sh
# Every import between the engine's packages, as Mermaid edges.
go list -f '{{.ImportPath}} {{join .Imports " "}}' ./... | grep -v queuetest |
  awk -v P=github.com/tecnosam/tailwise/ '{ src = substr($1, length(P) + 1); gsub("/", "_", src)
    for (i = 2; i <= NF; i++) if (index($i, P) == 1 && $i !~ /queuetest/) {
      dst = substr($i, length(P) + 1); gsub("/", "_", dst); print "    " src " --> " dst } }' | sort
```
