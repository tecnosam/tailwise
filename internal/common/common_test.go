package common

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPoolStopWaitsForWork(t *testing.T) {
	p := NewPool()
	finished := make(chan struct{})
	p.Start(1, func() {
		<-p.Stopped()
		time.Sleep(20 * time.Millisecond)
		close(finished)
	})
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Error("Stop returned before the work finished")
	}
	if !p.Stopping() {
		t.Error("Stopping = false after Stop")
	}
}

// When Stop runs out of time it cancels WorkCtx and still waits.
func TestPoolStopTimeoutCancelsWork(t *testing.T) {
	p := NewPool()
	p.Start(2, func() { <-p.WorkCtx().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Stop = %v, want the deadline", err)
	}
}

func TestHeaderValue(t *testing.T) {
	if got := HeaderValue("a\n  b\tc"); got != "a b c" {
		t.Errorf("HeaderValue = %q", got)
	}
	long := make([]byte, 2000)
	for i := range long {
		long[i] = 'x'
	}
	if got := HeaderValue(string(long)); len(got) != maxHeaderValue+3 {
		t.Errorf("length %d, want bounded", len(got))
	}
}
