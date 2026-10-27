package queuetest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/queue"
	"github.com/tecnosam/tailwise/internal/retry"
	"github.com/tecnosam/tailwise/internal/trace"
)

const WaitFor = 10 * time.Second

// Config is small, with fast retries and short polls.
func Config() queue.Config {
	return queue.Config{
		ConnectTimeout: 5 * time.Second,
		Stream:         "TRACES",
		Subject:        "traces.in",
		Consumer:       "workers",
		MaxMessages:    1000,
		AckWait:        30 * time.Second,
		Redelivery:     retry.Policy{Attempts: 3, Backoff: []time.Duration{20 * time.Millisecond}},
		DLQStream:      "TRACES_DLQ",
		DLQSubject:     "traces.dlq",
		DLQMaxAge:      time.Hour,
		PollWait:       100 * time.Millisecond,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func WithSpans(idle, maxWait time.Duration) func(*queue.Config) {
	return func(c *queue.Config) {
		c.Spans = queue.SpanConfig{
			Stream:         "SPANS",
			Subject:        "spans",
			MaxAge:         time.Hour,
			PendingStream:  "PENDING",
			PendingSubject: "pending",
			IdleTimeout:    idle,
			MaxWait:        maxWait,
		}
	}
}

// JoinBatches stands in for the OTLP assembler: the question is the batches
// joined in arrival order, and a batch saying "unscorable" makes the trace
// unscorable.
func JoinBatches(traceID string, batches [][]byte) (trace.Trace, error) {
	var parts []string
	for _, b := range batches {
		if string(b) == "unscorable" {
			return trace.Trace{}, errors.New("trace is not scorable: no question")
		}
		parts = append(parts, string(b))
	}
	return trace.Trace{TraceID: traceID, Question: strings.Join(parts, "+"), FinalAnswer: "a", OTLP: []byte("original spans")}, nil
}

func Sample(id string) trace.Trace {
	return trace.Trace{TraceID: id, Question: "q " + id, FinalAnswer: "a"}
}

func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(WaitFor)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type Collector struct {
	mu     sync.Mutex
	traces []trace.Trace
}

func (c *Collector) Handle(_ context.Context, t trace.Trace) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.traces = append(c.traces, t)
	return nil
}

func (c *Collector) Snapshot() []trace.Trace {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]trace.Trace(nil), c.traces...)
}

type Faults struct {
	mu   sync.Mutex
	errs []*fault.Error
}

func (f *Faults) Report(_ context.Context, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fe, _ := errors.AsType[*fault.Error](err)
	f.errs = append(f.errs, fe)
}

func (f *Faults) All() []*fault.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fault.Error(nil), f.errs...)
}

func (f *Faults) Ops() []fault.Op {
	var out []fault.Op
	for _, e := range f.All() {
		out = append(out, e.Op)
	}
	return out
}
