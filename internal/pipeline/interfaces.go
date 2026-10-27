package pipeline

import (
	"context"
	"time"

	"github.com/tecnosam/tailwise/internal/trace"
)

// A rejected request is fault.KindRejected; a model that is down, slow or
// answering garbage is fault.KindUnavailable or fault.KindTimeout.
type Scorer interface {
	Score(ctx context.Context, state trace.State) (Score, error)
	Ready(ctx context.Context) error
}

// Export reports whether it handed the trace on.
type Exporter interface {
	Export(ctx context.Context, rec Record) bool
}

// A full queue is fault.KindFull; one that cannot be reached is
// fault.KindUnavailable.
type Publisher interface {
	Publish(ctx context.Context, t trace.Trace) error
	Ready() error
}

type Metrics interface {
	ScoreFinished(ctx context.Context, r ScoreResult)
	TraceDecided(ctx context.Context, rec Record)
}

type ScoreResult struct {
	Agent    AgentLabel
	Model    ModelName
	Duration time.Duration // the whole call, retries included
	Err      error
}
