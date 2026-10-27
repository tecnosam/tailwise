package queue

import (
	"context"

	"github.com/tecnosam/tailwise/internal/trace"
)

// AbstractBroker is the trace queue and span store: the API and the assemblers
// publish to it, the OTLP receivers store spans in it, the workers take from it.
type AbstractBroker interface {
	// A full queue is fault.KindFull; any other failure fault.KindUnavailable.
	Publish(ctx context.Context, t trace.Trace) error
	PublishSpans(ctx context.Context, byTrace map[string][]byte) error
	StartWorkers(n int, handler Handler) Stopper
	StartAssemblers(n int, assemble AssembleFunc) Stopper
	Ready() error
	// Depth counts traces queued, waiting for redelivery, or being handled.
	Depth(ctx context.Context) (int64, error)
	// Close releases the broker. Stop its workers and assemblers first.
	Close()
}

type Stopper interface {
	// Stop stops after the work in flight, or when ctx ends.
	Stop(ctx context.Context) error
}
