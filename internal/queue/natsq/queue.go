// Package natsq is the trace queue and span store on NATS JetStream. Every
// engine sharing the streams shares the work.
package natsq

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/tecnosam/tailwise/internal/common"
	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/jsq"
	"github.com/tecnosam/tailwise/internal/observe"
	"github.com/tecnosam/tailwise/internal/queue"
	"github.com/tecnosam/tailwise/internal/trace"
)

const (
	setupTimeout   = 5 * time.Second
	setupRetryWait = 500 * time.Millisecond
	ackTimeout     = 5 * time.Second
	publishTimeout = 5 * time.Second

	defaultPollWait = 2 * time.Second
)

// A dead letter's body is the original payload, so it can be replayed by
// publishing it back to the trace subject.
const (
	HeaderTraceID        = jsq.HeaderTraceID
	HeaderDeadReason     = jsq.HeaderDeadReason
	HeaderDeadError      = jsq.HeaderDeadError
	HeaderDeadAt         = jsq.HeaderDeadAt
	HeaderDeliveries     = jsq.HeaderDeliveries
	HeaderSourceStream   = jsq.HeaderSourceStream
	HeaderSourceSequence = jsq.HeaderSourceSequence
)

type Queue struct {
	cfg      queue.Config
	conn     *common.Conn
	ownsConn bool // Close closes conn only when Connect opened it
	js       jetstream.JetStream
	work     *jsq.Queue[trace.Trace]
	logger   *slog.Logger

	// Set only when cfg.Spans is enabled.
	spans       jetstream.Stream
	pendingCons jetstream.Consumer
}

func Connect(ctx context.Context, url string, cfg queue.Config) (*Queue, error) {
	conn, err := common.DialNATS(common.NATSConfig{URL: url, Logger: cfg.Logger, Reporter: cfg.Reporter})
	if err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	q, err := New(ctx, conn, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	q.ownsConn = true
	return q, nil
}

// New keeps trying until cfg.ConnectTimeout, so the engine can start alongside
// NATS.
func New(ctx context.Context, conn *common.Conn, cfg queue.Config) (*Queue, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Reporter == nil {
		cfg.Reporter = observe.Discard
	}
	// The assemblers pull with it too, not only the trace workers.
	cfg.PollWait = cmp.Or(cfg.PollWait, defaultPollWait)
	q := &Queue{cfg: cfg, conn: conn, js: conn.JS, logger: cfg.Logger}
	if err := common.RetryUntil(ctx, cfg.ConnectTimeout, setupRetryWait, q.setup); err != nil {
		return nil, fmt.Errorf("queue: NATS JetStream not ready after %s: %w", cfg.ConnectTimeout, err)
	}
	return q, nil
}

func (q *Queue) setup(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	work, err := jsq.New(ctx, q.conn, q.workConfig(), decodeTrace)
	if err != nil {
		return err
	}
	q.work = work
	if q.cfg.Spans.Enabled() {
		return q.setupSpans(ctx)
	}
	return nil
}

func (q *Queue) workConfig() jsq.Config {
	c := q.cfg
	return jsq.Config{
		Stream:              c.Stream,
		Subject:             c.Subject,
		Description:         "Agent traces waiting for a keep/drop decision",
		MaxMsgs:             c.MaxMessages,
		Consumer:            c.Consumer,
		ConsumerDescription: "Tailwise engine worker pool",
		AckWait:             c.AckWait,
		DLQStream:           c.DLQStream,
		DLQSubject:          c.DLQSubject,
		DLQDescription:      "Traces that could not be scored because of system errors",
		DLQMaxAge:           c.DLQMaxAge,
		Retry:               c.Redelivery,
		PollWait:            c.PollWait,
		Component:           fault.Queue,
		PublishOp:           "publish trace",
		RetryOp:             "process trace, will redeliver",
		DeadLetterOp:        "dead-letter trace",
		Reporter:            c.Reporter,
	}
}

func decodeTrace(data []byte) (trace.Trace, error) { return trace.Decode(bytes.NewReader(data)) }

func (q *Queue) Publish(ctx context.Context, t trace.Trace) error {
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("queue: encode trace: %w", err)
	}
	return q.work.Publish(ctx, t.TraceID, data)
}

func (q *Queue) StartWorkers(n int, handler queue.Handler) queue.Stopper {
	return q.work.Start(n, func(ctx context.Context, t trace.Trace, d *jsq.Delivery) error {
		if t.TraceID == "" {
			// Published straight to the subject, without the API's ID.
			t.TraceID = queue.SequenceID(q.cfg.Stream, d.Sequence)
		}
		d.TraceID = t.TraceID
		return handler(ctx, t)
	})
}

func (q *Queue) Depth(ctx context.Context) (int64, error) { return q.work.Depth(ctx) }

func (q *Queue) Ready() error { return q.conn.Ready() }

func (q *Queue) report(err *fault.Error) {
	q.cfg.Reporter.Report(context.Background(), err)
}

func (q *Queue) Close() {
	if q.ownsConn {
		q.conn.Close()
	}
}

var _ queue.AbstractBroker = (*Queue)(nil)
