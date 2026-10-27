// Package export sends each kept trace to every configured back end, each from
// its own queue, so a slow or unreachable one never holds up scoring.
package export

import (
	"context"

	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/retry"
)

type AbstractBackend interface {
	// Send returns once the destination has the trace: an error means it was not
	// delivered.
	Send(ctx context.Context, rec pipeline.Record) error
	Shutdown(ctx context.Context) error
}

type BackendConfig struct {
	Name               string
	Endpoint           string // the full OTLP/HTTP traces URL
	Headers            map[string]string
	ResourceAttributes map[string]string
	QueueSize          int // 0 means DefaultQueueSize
	Concurrency        int // 0 means DefaultConcurrency
	// Zero Attempts or nil Backoff take DefaultRetry's.
	Retry retry.Policy
}

const (
	DefaultQueueSize   = 1024
	DefaultConcurrency = 4
)

type BackendFunc func(ctx context.Context, cfg BackendConfig) (AbstractBackend, error)
