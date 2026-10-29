// Package natsq is the export queues on NATS JetStream: a queued record
// survives a restart and a back-end outage, and engines sharing the streams
// share the sending.
package natsq

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tecnosam/tailwise/internal/common"
	"github.com/tecnosam/tailwise/internal/export"
	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/jsq"
	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/trace"
)

const (
	// Short: the export workers stop last, after the trace workers.
	defaultPollWait = 500 * time.Millisecond
	// Outlasts the longest send (one 10s request), so a record is not handed to
	// a second worker while the first is still sending it.
	ackWait = time.Minute

	headerTraceID    = jsq.HeaderTraceID
	headerDeliveries = jsq.HeaderDeliveries
	HeaderExporter   = "Tailwise-Exporter"
	headerExporter   = HeaderExporter
)

// Each back end gets its own stream <Stream>_<NAME> on <Subject>.<name>, read
// by the consumer <Consumer>-<name>; given-up records share DLQStream.
type Streams struct {
	Stream     string
	Subject    string
	Consumer   string
	DLQStream  string
	DLQSubject string
	DLQMaxAge  time.Duration
	PollWait   time.Duration
}

// Names that map to the same stream clash.
func StreamName(prefix, exporter string) string {
	return prefix + "_" + strings.ToUpper(strings.ReplaceAll(exporter, "-", "_"))
}

func Queues(conn *common.Conn, streams Streams) export.QueueFunc {
	return func(ctx context.Context, cfg export.QueueConfig) (export.AbstractQueue, error) {
		return Open(ctx, conn, streams, cfg)
	}
}

func Open(ctx context.Context, conn *common.Conn, s Streams, cfg export.QueueConfig) (export.AbstractQueue, error) {
	cfg = cfg.WithDefaults()
	work, err := jsq.New(ctx, conn, jsq.Config{
		Stream:              StreamName(s.Stream, cfg.Name),
		Subject:             s.Subject + "." + cfg.Name,
		Description:         "Kept traces waiting to be sent to the " + cfg.Name + " back end",
		MaxMsgs:             int64(cfg.Size),
		Consumer:            s.Consumer + "-" + cfg.Name,
		ConsumerDescription: "Tailwise engine exporter " + cfg.Name,
		AckWait:             ackWait,
		DLQStream:           s.DLQStream,
		DLQSubject:          s.DLQSubject,
		DLQDescription:      "Kept traces that could not be sent to a back end",
		DLQMaxAge:           s.DLQMaxAge,
		Retry:               cfg.Retry,
		Retryable:           fault.Retryable, // corrupt stored spans are given up at once
		PollWait:            cmp.Or(s.PollWait, defaultPollWait),
		Component:           fault.Export,
		PublishOp:           "queue trace for export",
		RetryOp:             "export trace, will retry",
		DeadLetterOp:        "export trace, dead-lettered",
		Attrs:               map[string]any{"exporter": cfg.Name},
		DeadHeaders:         map[string]string{HeaderExporter: cfg.Name},
		Reporter:            cfg.Reporter,
	}, decodeRecord)
	if err != nil {
		return nil, fmt.Errorf("export queue %s: %w", cfg.Name, err)
	}
	q := &natsQueue{work: work, maxPayload: conn.MaxPayload()}
	q.workers = work.Start(cfg.Concurrency, func(ctx context.Context, rec pipeline.Record, _ *jsq.Delivery) error {
		return cfg.Send(ctx, rec)
	})
	return q, nil
}

type natsQueue struct {
	work       *jsq.Queue[pipeline.Record]
	workers    *jsq.Workers
	maxPayload int
}

func (q *natsQueue) Enqueue(ctx context.Context, rec pipeline.Record) error {
	if q.workers.Stopping() {
		return fault.Unavailable(fault.Export, "queue trace for export", export.ErrShutDown)
	}
	data, err := encodeRecord(rec, q.maxPayload)
	if err != nil {
		return fault.Invalid(fault.Export, "encode trace for export", err)
	}
	return q.work.Publish(ctx, rec.Trace.TraceID, data)
}

func (q *natsQueue) Depth(ctx context.Context) (int64, error) { return q.work.Depth(ctx) }

func (q *natsQueue) Flush(ctx context.Context) error { return q.work.Flush(ctx) }

// What is still queued stays in JetStream for the next start.
func (q *natsQueue) Shutdown(ctx context.Context) error { return q.workers.Stop(ctx) }

type recordMessage struct {
	Trace     trace.Trace `json:"trace"`
	Agent     string      `json:"agent"`
	Keep      bool        `json:"keep"`
	Scored    bool        `json:"scored"`
	PKeep     float64     `json:"p_keep"`
	Rule      string      `json:"rule"`
	Reason    string      `json:"reason"`
	Cause     string      `json:"cause,omitempty"`
	Severity  float64     `json:"severity,omitempty"`
	Model     string      `json:"model"`
	LatencyNS int64       `json:"latency_ns"`

	Answers map[string]pipeline.Probability `json:"answers,omitempty"`
	Flags   []string                        `json:"flags,omitempty"`
}

// A trace too big for the server keeps its decision but loses its original
// spans, and is exported rebuilt.
func encodeRecord(rec pipeline.Record, maxPayload int) ([]byte, error) {
	d := rec.Decision
	m := recordMessage{
		Trace: rec.Trace, Agent: string(rec.Agent), Keep: d.Keep, Scored: d.Scored, PKeep: float64(d.PKeep),
		Rule: string(d.Rule), Reason: d.Reason, Cause: d.Cause, Severity: d.Severity, Model: string(rec.Model), LatencyNS: int64(rec.Latency),
		Answers: rec.Answers, Flags: rec.Flags,
	}
	data, err := json.Marshal(m)
	if err != nil || maxPayload <= 0 || len(data) <= maxPayload || len(m.Trace.OTLP) == 0 {
		return data, err
	}
	m.Trace.OTLP = nil
	return json.Marshal(m)
}

func decodeRecord(data []byte) (pipeline.Record, error) {
	var m recordMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return pipeline.Record{}, err
	}
	return pipeline.Record{
		Trace: m.Trace,
		Agent: pipeline.AgentLabel(m.Agent),
		Decision: pipeline.Decision{
			Keep: m.Keep, Scored: m.Scored, PKeep: pipeline.Probability(m.PKeep),
			Rule: pipeline.Rule(m.Rule), Reason: m.Reason, Cause: m.Cause, Severity: m.Severity,
		},
		Model:   pipeline.ModelName(m.Model),
		Latency: time.Duration(m.LatencyNS),
		Answers: m.Answers,
		Flags:   m.Flags,
	}, nil
}
