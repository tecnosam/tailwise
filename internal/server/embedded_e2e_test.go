package server

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tecnosam/tailwise/internal/broker"
	"github.com/tecnosam/tailwise/internal/queue"
	"github.com/tecnosam/tailwise/internal/retry"
)

// embeddedBroker is the queue main uses when queue.url is empty: JetStream
// on a NATS server embedded in the process.
func embeddedBroker(t *testing.T) queue.AbstractBroker {
	t.Helper()
	b, err := broker.New(broker.Config{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	q, err := b.Ingest(context.Background(), queue.Config{
		ConnectTimeout: 5 * time.Second,
		Stream:         "TRACES", Subject: "traces.in", Consumer: "workers", MaxMessages: 100,
		AckWait: 30 * time.Second, Redelivery: retry.Policy{Attempts: 2, Backoff: []time.Duration{20 * time.Millisecond}},
		DLQStream: "TRACES_DLQ", DLQSubject: "traces.dlq", DLQMaxAge: time.Hour,
		Spans: queue.SpanConfig{
			Stream: "SPANS", Subject: "spans", MaxAge: time.Hour,
			PendingStream: "PENDING", PendingSubject: "pending",
			IdleTimeout: 300 * time.Millisecond, MaxWait: 10 * time.Second,
		},
		PollWait: 100 * time.Millisecond,
		Logger:   quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	return q
}

// The whole OTLP path without a NATS server to run: spans over gRPC and
// HTTP, assembled on the embedded server, scored, and the kept trace
// forwarded as its original spans.
func TestEmbeddedOTLPPipelineKeepsAndDrops(t *testing.T) {
	kev := newFakeKev(t, func(w http.ResponseWriter, state string) {
		if strings.Contains(state, "Lyon") {
			w.Write(readFile(t, "kev", "success.json")) // 0.93: keep
			return
		}
		w.Write(readFile(t, "kev", "success_low.json")) // 0.12: drop
	})
	p := newOTLPPipelineOn(t, kev, embeddedBroker(t))

	wrong := fixtureSpans(t, "wrong_river.json", wrongID)
	p.exportGRPC(t, wrong[1:]...)
	p.exportHTTP(t, wrong[0])
	p.exportGRPC(t, fixtureSpans(t, "correct_river.json", correctID)...)

	waitUntil(t, "both traces to be scored", func() bool { return kev.calls.Load() == 2 })
	waitUntil(t, "the kept trace to be forwarded", func() bool { return p.phoenix.count() == 1 })
	time.Sleep(100 * time.Millisecond) // nothing else follows

	p.phoenix.mu.Lock()
	defer p.phoenix.mu.Unlock()
	if len(p.phoenix.uploads) != 1 {
		t.Fatalf("%d traces forwarded, want only the kept one", len(p.phoenix.uploads))
	}
	for _, rs := range p.phoenix.uploads[0] {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				if hex.EncodeToString(s.TraceId) != hex.EncodeToString(wrongID) {
					t.Errorf("forwarded span %q has trace ID %x, want %x", s.Name, s.TraceId, wrongID)
				}
			}
		}
	}
}

// The JSON API through the embedded queue: accepted, scored by a worker,
// and the kept trace exported.
func TestEmbeddedQueuedPipeline(t *testing.T) {
	kev := newFakeKev(t, func(w http.ResponseWriter, _ string) {
		w.Write(readFile(t, "kev", "success.json"))
	})
	p := newOTLPPipelineOn(t, kev, embeddedBroker(t))

	if w := p.do("POST", "/v1/traces", string(readFile(t, "traces", "wrong_river.json"))); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	waitUntil(t, "the trace to be scored", func() bool { return kev.calls.Load() == 1 })
	waitUntil(t, "the kept trace to be exported", func() bool { return len(p.phoenix.Ended()) == 3 })
	if a := rootAttrs(t, p.phoenix); a["eval.decision"].AsString() != "keep" {
		t.Errorf("eval.decision = %v", a["eval.decision"])
	}
}
