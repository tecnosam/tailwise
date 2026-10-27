package metrics

import (
	"context"

	"github.com/tecnosam/tailwise/internal/pipeline"
)

type Noop struct{}

func (Noop) RequestFinished(context.Context, Request)            {}
func (Noop) ScoreFinished(context.Context, pipeline.ScoreResult) {}
func (Noop) TraceDecided(context.Context, pipeline.Record)       {}
func (Noop) SendFinished(context.Context, Send)                  {}
func (Noop) ErrorReported(context.Context, string, string)       {}
