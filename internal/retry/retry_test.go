package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPolicy(t *testing.T) {
	p := Policy{Attempts: 3, Backoff: []time.Duration{time.Second, 5 * time.Second}}
	for attempts, want := range map[int]time.Duration{0: time.Second, 1: time.Second, 2: 5 * time.Second, 9: 5 * time.Second} {
		if got := p.Delay(attempts); got != want {
			t.Errorf("Delay(%d) = %v, want %v", attempts, got, want)
		}
	}
	if p.Exhausted(2) || !p.Exhausted(3) {
		t.Error("Exhausted is wrong around Attempts")
	}
	if (Policy{}).Delay(1) != 0 {
		t.Error("no backoff should mean no wait")
	}
}

func TestJitterStaysBetweenHalfAndAll(t *testing.T) {
	p := Policy{Backoff: []time.Duration{time.Second}, Jitter: true}
	for range 1000 {
		if d := p.Delay(1); d < 500*time.Millisecond || d > time.Second {
			t.Fatalf("Delay = %v, want within [500ms, 1s]", d)
		}
	}
}

func TestSleepStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep = %v, want context.Canceled", err)
	}
}
