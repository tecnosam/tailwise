// Package retry is the retry policy the queues, the export queues and the
// synchronous endpoint share.
package retry

import (
	"context"
	"math/rand/v2"
	"time"
)

type Policy struct {
	// Attempts counts every try, the first included.
	Attempts int
	// Backoff[i] is the wait after try i+1; the last value repeats.
	Backoff []time.Duration
	// Jitter waits between half and all of each delay, so clients that failed
	// together do not retry together.
	Jitter bool
}

func (p Policy) Exhausted(attempts int) bool { return attempts >= p.Attempts }

func (p Policy) Delay(attempts int) time.Duration {
	if len(p.Backoff) == 0 {
		return 0
	}
	d := p.Backoff[min(max(attempts-1, 0), len(p.Backoff)-1)]
	if p.Jitter && d > 0 {
		d = d/2 + rand.N(d/2+1)
	}
	return d
}

func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
