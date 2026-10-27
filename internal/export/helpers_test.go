package export_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/tecnosam/tailwise/internal/broker"
	"github.com/tecnosam/tailwise/internal/common"
	"github.com/tecnosam/tailwise/internal/export"
	"github.com/tecnosam/tailwise/internal/export/natsq"
	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/queue/queuetest"
	"github.com/tecnosam/tailwise/internal/trace"
)

var keepHigh = pipeline.Decision{Keep: true, Scored: true, PKeep: 0.95, Rule: pipeline.RuleAboveThreshold}

func kept(id string) pipeline.Record {
	return pipeline.Record{Trace: trace.Trace{TraceID: id}, Decision: keepHigh}
}

// recordingReporter keeps every reported error, for assertions.
type recordingReporter struct {
	mu   sync.Mutex
	errs []error
}

func (r *recordingReporter) Report(_ context.Context, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, err)
}

func (r *recordingReporter) all() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.errs...)
}

// natsQueues is a JetStream export-queue factory on an in-process server.
func natsQueues(t *testing.T) (export.QueueFunc, jetstream.JetStream) {
	t.Helper()
	srv := queuetest.StartServer(t)
	conn, err := common.DialNATS(common.NATSConfig{URL: srv.ClientURL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	streams := natsq.Streams{
		Stream: "EXPORTS", Subject: "export", Consumer: "tailwise",
		DLQStream: "EXPORTS_DLQ", DLQSubject: "export_dlq", DLQMaxAge: time.Hour,
		PollWait: 100 * time.Millisecond,
	}
	return natsq.Queues(conn, streams), conn.JS
}

// embeddedQueues is a JetStream export-queue factory on a NATS server
// embedded in the test process, as the engine runs without queue.url.
func embeddedQueues(t *testing.T) export.QueueFunc {
	t.Helper()
	b, err := broker.New(broker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	streams := natsq.Streams{
		Stream: "EXPORTS", Subject: "export", Consumer: "tailwise",
		DLQStream: "EXPORTS_DLQ", DLQSubject: "export_dlq", DLQMaxAge: time.Hour,
		PollWait: 100 * time.Millisecond,
	}
	return b.ExportQueues(streams)
}
