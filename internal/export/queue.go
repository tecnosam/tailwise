package export

import (
	"cmp"
	"context"
	"errors"
	"time"

	"github.com/tecnosam/tailwise/internal/observe"
	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/retry"
)

var ErrShutDown = errors.New("export queue is shut down")

var DefaultRetry = retry.Policy{Attempts: 3, Backoff: []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}}

type SendFunc func(ctx context.Context, rec pipeline.Record) error

type QueueConfig struct {
	Name        string // names its stream on NATS
	Size        int    // past it Enqueue is refused
	Concurrency int
	// Errors a retry cannot fix are given up at once.
	Retry    retry.Policy
	Send     SendFunc
	Reporter observe.Reporter
}

type AbstractQueue interface {
	// A full queue is fault.KindFull.
	Enqueue(ctx context.Context, rec pipeline.Record) error
	Flush(ctx context.Context) error
	Shutdown(ctx context.Context) error
	// Depth counts records waiting to be sent, retries included.
	Depth(ctx context.Context) (int64, error)
}

type QueueFunc func(ctx context.Context, cfg QueueConfig) (AbstractQueue, error)

func (cfg QueueConfig) WithDefaults() QueueConfig {
	cfg.Size = cmp.Or(cfg.Size, DefaultQueueSize)
	cfg.Concurrency = cmp.Or(cfg.Concurrency, DefaultConcurrency)
	cfg.Retry.Attempts = cmp.Or(cfg.Retry.Attempts, DefaultRetry.Attempts)
	if cfg.Retry.Backoff == nil {
		cfg.Retry.Backoff = DefaultRetry.Backoff
	}
	if cfg.Reporter == nil {
		cfg.Reporter = observe.Discard
	}
	return cfg
}
