// Package jsq is the JetStream work queue both the trace queue and the export
// queues run on: a work-queue stream, a durable consumer pulled one message at
// a time, and a dead-letter stream. A failed message is redelivered with a
// delay until its retry policy runs out, then dead-lettered.
package jsq

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/tecnosam/tailwise/internal/common"
	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/observe"
	"github.com/tecnosam/tailwise/internal/retry"
)

// A dead letter's body is the original payload, so it can be replayed by
// publishing it back.
const (
	HeaderTraceID        = "Tailwise-Trace-Id"
	HeaderDeadReason     = "Tailwise-Dead-Reason"
	HeaderDeadError      = "Tailwise-Dead-Error"
	HeaderDeadAt         = "Tailwise-Dead-At"
	HeaderDeliveries     = "Tailwise-Deliveries"
	HeaderSourceStream   = "Tailwise-Source-Stream"
	HeaderSourceSequence = "Tailwise-Source-Sequence"
)

const (
	ReasonSystemError    = "system_error"
	ReasonGivenUp        = "given_up"
	ReasonInvalidPayload = "invalid_payload"
)

const (
	publishTimeout  = 5 * time.Second
	ackTimeout      = 5 * time.Second
	flushPoll       = 25 * time.Millisecond
	defaultPollWait = 2 * time.Second
)

type Config struct {
	Stream      string
	Subject     string
	Description string
	MaxMsgs     int64

	Consumer            string
	ConsumerDescription string
	AckWait             time.Duration

	DLQStream      string
	DLQSubject     string
	DLQDescription string
	DLQMaxAge      time.Duration

	Retry retry.Policy
	// Retryable says whether an error is worth another delivery; nil means every
	// error is.
	Retryable func(error) bool
	PollWait  time.Duration

	Component    fault.Component
	PublishOp    fault.Op
	RetryOp      fault.Op
	DeadLetterOp fault.Op
	Attrs        map[string]any
	DeadHeaders  map[string]string
	Reporter     observe.Reporter
}

type Delivery struct {
	// TraceID starts as the message's header; a handler may fill it in.
	TraceID  string
	Attempt  int
	Sequence uint64
}

// Handler processes one message. Returning nil acknowledges it.
type Handler[T any] func(ctx context.Context, v T, d *Delivery) error

type Queue[T any] struct {
	cfg    Config
	conn   *common.Conn
	cons   jetstream.Consumer
	decode func([]byte) (T, error)
}

// New tries once; a caller starting alongside NATS wraps it in
// common.RetryUntil.
func New[T any](ctx context.Context, conn *common.Conn, cfg Config, decode func([]byte) (T, error)) (*Queue[T], error) {
	if cfg.Reporter == nil {
		cfg.Reporter = observe.Discard
	}
	if cfg.PollWait <= 0 {
		cfg.PollWait = defaultPollWait
	}
	if cfg.Retryable == nil {
		cfg.Retryable = func(error) bool { return true }
	}
	err := conn.EnsureStreams(ctx,
		jetstream.StreamConfig{
			// DiscardNew: a full queue refuses work instead of evicting unhandled work.
			Name: cfg.Stream, Description: cfg.Description, Subjects: []string{cfg.Subject},
			Retention: jetstream.WorkQueuePolicy, Storage: jetstream.FileStorage,
			MaxMsgs: cfg.MaxMsgs, Discard: jetstream.DiscardNew,
		},
		jetstream.StreamConfig{
			Name: cfg.DLQStream, Description: cfg.DLQDescription, Subjects: []string{cfg.DLQSubject},
			Retention: jetstream.LimitsPolicy, Storage: jetstream.FileStorage, MaxAge: cfg.DLQMaxAge,
		})
	if err != nil {
		return nil, err
	}
	// MaxDeliver stays unlimited on the server and the workers dead-letter
	// themselves: a server-side limit would strand a message if a worker died on
	// its last delivery.
	cons, err := conn.JS.CreateOrUpdateConsumer(ctx, cfg.Stream, jetstream.ConsumerConfig{
		Durable:       cfg.Consumer,
		Description:   cfg.ConsumerDescription,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       cfg.AckWait,
		MaxDeliver:    -1,
		FilterSubject: cfg.Subject,
	})
	if err != nil {
		return nil, fmt.Errorf("consumer %s: %w", cfg.Consumer, err)
	}
	return &Queue[T]{cfg: cfg, conn: conn, cons: cons, decode: decode}, nil
}

func (q *Queue[T]) Publish(ctx context.Context, traceID string, data []byte, opts ...jetstream.PublishOpt) error {
	msg := nats.NewMsg(q.cfg.Subject)
	msg.Data = data
	msg.Header.Set(HeaderTraceID, traceID)
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	if _, err := q.conn.JS.PublishMsg(ctx, msg, opts...); err != nil {
		kind := fault.KindUnavailable
		if common.IsStreamFull(err) {
			kind = fault.KindFull
		}
		return q.fault(kind, q.cfg.PublishOp, err).WithTrace(traceID)
	}
	return nil
}

func (q *Queue[T]) Depth(ctx context.Context) (int64, error) {
	info, err := q.cons.Info(ctx)
	if err != nil {
		return 0, err
	}
	return int64(info.NumPending) + int64(info.NumAckPending), nil
}

func (q *Queue[T]) Flush(ctx context.Context) error {
	for {
		n, err := q.Depth(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		if err := retry.Sleep(ctx, flushPoll); err != nil {
			return err
		}
	}
}

// Workers stops pulling on Stop and waits for messages in flight; if Stop's
// ctx ends first, they are cancelled and returned.
type Workers struct {
	*common.Pool
}

func (q *Queue[T]) Start(n int, h Handler[T]) *Workers {
	w := &Workers{Pool: common.NewPool()}
	w.Start(n, func() {
		common.Pull(w.Pool, q.cons, q.cfg.PollWait, func(msg jetstream.Msg) { q.handle(w.Pool, msg, h) },
			func(err error) { q.report(q.fault(fault.KindUnavailable, "pull", err)) })
	})
	return w
}

func (q *Queue[T]) handle(p *common.Pool, msg jetstream.Msg, h Handler[T]) {
	d := &Delivery{TraceID: msg.Headers().Get(HeaderTraceID), Attempt: 1}
	if meta, err := msg.Metadata(); err == nil {
		d.Attempt, d.Sequence = int(meta.NumDelivered), meta.Sequence.Stream
	}
	v, err := q.decode(msg.Data())
	if err != nil {
		q.deadLetter(msg, d, ReasonInvalidPayload, fault.Invalid(q.cfg.Component, "decode message", err))
		return
	}
	err = h(p.WorkCtx(), v, d)
	switch {
	case err == nil:
		q.ack(msg, d)
	case p.WorkCtx().Err() != nil:
		// Shutdown ran out of time mid-message: hand it straight back.
		if err := msg.Nak(); err != nil {
			q.report(q.fault(fault.KindUnavailable, "return message on shutdown", err).WithTrace(d.TraceID))
		}
	case !q.cfg.Retryable(err):
		q.deadLetter(msg, d, ReasonGivenUp, err)
	case q.cfg.Retry.Exhausted(d.Attempt):
		q.deadLetter(msg, d, ReasonSystemError, err)
	default:
		delay := q.cfg.Retry.Delay(d.Attempt)
		q.report(q.fault(fault.KindOf(err), q.cfg.RetryOp, err).WithTrace(d.TraceID).
			With("delivery", d.Attempt).With("max_deliver", q.cfg.Retry.Attempts).With("retry_in", delay.String()))
		q.nak(msg, d, delay)
	}
}

// deadLetter removes the message only once the dead-letter copy is
// confirmed; if the copy fails it stays queued.
func (q *Queue[T]) deadLetter(msg jetstream.Msg, d *Delivery, reason string, cause error) {
	dead := nats.NewMsg(q.cfg.DLQSubject)
	dead.Data = msg.Data()
	for k, v := range q.cfg.DeadHeaders {
		dead.Header.Set(k, v)
	}
	dead.Header.Set(HeaderTraceID, d.TraceID)
	dead.Header.Set(HeaderDeadReason, reason)
	dead.Header.Set(HeaderDeadError, common.HeaderValue(cause.Error()))
	dead.Header.Set(HeaderDeadAt, time.Now().UTC().Format(time.RFC3339))
	dead.Header.Set(HeaderDeliveries, strconv.Itoa(d.Attempt))
	dead.Header.Set(HeaderSourceStream, q.cfg.Stream)
	dead.Header.Set(HeaderSourceSequence, strconv.FormatUint(d.Sequence, 10))

	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if _, err := q.conn.JS.PublishMsg(ctx, dead); err != nil {
		delay := q.cfg.Retry.Delay(d.Attempt)
		q.report(q.fault(fault.KindUnavailable, "dead-letter, it stays queued", err).AsSevere().
			WithTrace(d.TraceID).With("retry_in", delay.String()))
		q.nak(msg, d, delay)
		return
	}
	q.report(q.fault(fault.KindOf(cause), q.cfg.DeadLetterOp, cause).AsSevere().WithTrace(d.TraceID).
		With("reason", reason).With("deliveries", d.Attempt).With("dlq_stream", q.cfg.DLQStream))
	q.ack(msg, d)
}

func (q *Queue[T]) nak(msg jetstream.Msg, d *Delivery, delay time.Duration) {
	if err := msg.NakWithDelay(delay); err != nil {
		q.report(q.fault(fault.KindUnavailable, "return message to queue", err).WithTrace(d.TraceID))
	}
}

// A lost ack means the message is handled twice, so the ack is confirmed.
func (q *Queue[T]) ack(msg jetstream.Msg, d *Delivery) {
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	if err := msg.DoubleAck(ctx); err != nil {
		q.report(q.fault(fault.KindUnavailable, "ack, it may be delivered again", err).AsSevere().WithTrace(d.TraceID))
	}
}

func (q *Queue[T]) fault(kind fault.Kind, op fault.Op, err error) *fault.Error {
	f := fault.New(kind, q.cfg.Component, op, err)
	for _, k := range slices.Sorted(maps.Keys(q.cfg.Attrs)) {
		f = f.With(k, q.cfg.Attrs[k])
	}
	return f
}

func (q *Queue[T]) report(err *fault.Error) {
	q.cfg.Reporter.Report(context.Background(), err)
}
