// Package queue holds the trace queue's interfaces and settings; natsq
// implements them on JetStream.
package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strconv"
	"time"

	"github.com/tecnosam/tailwise/internal/jsq"
	"github.com/tecnosam/tailwise/internal/observe"
	"github.com/tecnosam/tailwise/internal/retry"
	"github.com/tecnosam/tailwise/internal/trace"
)

type Config struct {
	// After ConnectTimeout the client keeps reconnecting on its own.
	ConnectTimeout time.Duration

	Stream      string
	Subject     string
	Consumer    string
	MaxMessages int64

	AckWait    time.Duration
	Redelivery retry.Policy

	DLQStream  string
	DLQSubject string
	DLQMaxAge  time.Duration

	// Spans is zero when the OTLP receivers are off.
	Spans SpanConfig

	// PollWait bounds how long an idle worker takes to notice a stop. Defaults to 2s.
	PollWait time.Duration

	Logger   *slog.Logger
	Reporter observe.Reporter
}

type SpanConfig struct {
	// Spans of a trace go to <Subject>.<trace id>.
	Stream  string
	Subject string
	MaxAge  time.Duration

	// PendingStream holds one marker per trace with spans waiting.
	PendingStream  string
	PendingSubject string

	IdleTimeout time.Duration
	MaxWait     time.Duration
}

func (c SpanConfig) Enabled() bool { return c.Stream != "" }

// Handler returns an error only for a system error, so the trace is tried
// again later.
type Handler func(ctx context.Context, t trace.Trace) error

// An AssembleFunc error means the spans make no trace worth scoring; they are
// discarded.
type AssembleFunc func(traceID string, batches [][]byte) (trace.Trace, error)

const (
	ReasonSystemError    = jsq.ReasonSystemError
	ReasonInvalidPayload = jsq.ReasonInvalidPayload
)

const (
	MaxPendingAssemblies = 100_000
	AssemblerRetryDelay  = 5 * time.Second
)

// SequenceID derives a stable trace ID from a trace's place in its queue, for
// a trace published without one.
func SequenceID(queue string, sequence uint64) string {
	sum := sha256.Sum256([]byte(queue + ":" + strconv.FormatUint(sequence, 10)))
	return hex.EncodeToString(sum[:16])
}
