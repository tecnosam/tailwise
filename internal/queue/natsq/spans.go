package natsq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/tecnosam/tailwise/internal/common"
	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/queue"
	"github.com/tecnosam/tailwise/internal/trace"
)

const (
	assemblerAckWait    = 30 * time.Second
	assemblerOpTimeout  = 10 * time.Second
	assemblerMinRecheck = 50 * time.Millisecond
)

func (q *Queue) setupSpans(ctx context.Context) error {
	sc := q.cfg.Spans
	if err := q.conn.EnsureStreams(ctx, q.spanStream(), q.pendingStream()); err != nil {
		return err
	}
	spans, err := q.js.Stream(ctx, sc.Stream)
	if err != nil {
		return fmt.Errorf("stream %s: %w", sc.Stream, err)
	}
	q.spans = spans

	// A marker stays unacknowledged while its trace still receives spans, so the
	// limit on pending markers is the limit on traces in flight.
	cons, err := q.js.CreateOrUpdateConsumer(ctx, sc.PendingStream, jetstream.ConsumerConfig{
		Durable:       q.cfg.Consumer + "-assemblers",
		Description:   "Tailwise engine trace assemblers",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       assemblerAckWait,
		MaxDeliver:    -1,
		MaxAckPending: queue.MaxPendingAssemblies,
	})
	if err != nil {
		return fmt.Errorf("consumer %s-assemblers: %w", q.cfg.Consumer, err)
	}
	q.pendingCons = cons
	return nil
}

// MaxAge is the backstop for spans whose trace never completes.
func (q *Queue) spanStream() jetstream.StreamConfig {
	sc := q.cfg.Spans
	return jetstream.StreamConfig{
		Name:        sc.Stream,
		Description: "OTLP spans waiting to be assembled into traces, one subject per trace ID",
		Subjects:    []string{sc.Subject + ".*"},
		Retention:   jetstream.LimitsPolicy,
		Storage:     jetstream.FileStorage,
		MaxAge:      sc.MaxAge,
	}
}

// One marker per trace: a second one is refused, so a trace is assembled by
// one assembler at a time however many batches arrive.
func (q *Queue) pendingStream() jetstream.StreamConfig {
	sc := q.cfg.Spans
	return jetstream.StreamConfig{
		Name:                 sc.PendingStream,
		Description:          "Traces with spans waiting to be assembled, one marker per trace ID",
		Subjects:             []string{sc.PendingSubject + ".*"},
		Retention:            jetstream.WorkQueuePolicy,
		Storage:              jetstream.FileStorage,
		MaxMsgsPerSubject:    1,
		Discard:              jetstream.DiscardNew,
		DiscardNewPerSubject: true,
	}
}

func (q *Queue) PublishSpans(ctx context.Context, byTrace map[string][]byte) error {
	if !q.cfg.Spans.Enabled() {
		return errors.New("queue: span storage is not configured")
	}
	sc := q.cfg.Spans
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	maxPayload := q.conn.MaxPayload()
	var stored []string
	var acks []jetstream.PubAckFuture
	for traceID, data := range byTrace {
		if maxPayload > 0 && len(data) > maxPayload {
			// Retrying cannot help, so do not make the exporter retry.
			q.report(fault.Invalid(fault.Queue, "store spans", errors.New("span batch exceeds the NATS payload limit and was dropped")).
				AsSevere().WithTrace(traceID).With("bytes", len(data)).With("max_payload", maxPayload))
			continue
		}
		ack, err := q.js.PublishMsgAsync(&nats.Msg{Subject: sc.Subject + "." + traceID, Data: data})
		if err != nil {
			return fault.Unavailable(fault.Queue, "store spans", err)
		}
		stored = append(stored, traceID)
		acks = append(acks, ack)
	}
	if err := waitAcks(ctx, acks, false); err != nil {
		return err
	}

	// Markers go out only after the spans are stored, so an assembler never finds
	// a marker whose spans have not landed yet.
	acks = acks[:0]
	for _, traceID := range stored {
		ack, err := q.js.PublishMsgAsync(&nats.Msg{Subject: sc.PendingSubject + "." + traceID})
		if err != nil {
			return fault.Unavailable(fault.Queue, "store spans", err)
		}
		acks = append(acks, ack)
	}
	return waitAcks(ctx, acks, true)
}

// With markers set, a refusal because the trace already has one is success.
func waitAcks(ctx context.Context, acks []jetstream.PubAckFuture, markers bool) error {
	for _, ack := range acks {
		select {
		case <-ack.Ok():
		case err := <-ack.Err():
			if markers && common.IsSubjectFull(err) {
				continue
			}
			return fault.Unavailable(fault.Queue, "store spans", err)
		case <-ctx.Done():
			return fault.Timeout(fault.Queue, "store spans", ctx.Err())
		}
	}
	return nil
}

type Assemblers struct {
	*common.Pool
	q        *Queue
	assemble queue.AssembleFunc
}

// Each assembler takes a marker, waits until its trace has been quiet for
// IdleTimeout (or MaxWait has passed), and queues the assembled trace.
func (q *Queue) StartAssemblers(n int, assemble queue.AssembleFunc) queue.Stopper {
	a := &Assemblers{Pool: common.NewPool(), q: q, assemble: assemble}
	a.Start(n, func() {
		common.Pull(a.Pool, q.pendingCons, q.cfg.PollWait, a.handle, func(err error) {
			q.report(fault.Unavailable(fault.Queue, "pull pending trace", err))
		})
	})
	return a
}

func (a *Assemblers) handle(marker jetstream.Msg) {
	ctx, cancel := context.WithTimeout(context.Background(), assemblerOpTimeout)
	defer cancel()
	newAssembly(ctx, a, marker).run()
}

// assembly is one attempt: it acknowledges the marker, or puts it back with a
// delay.
type assembly struct {
	ctx     context.Context
	a       *Assemblers
	marker  jetstream.Msg
	traceID string
	subject string
}

func newAssembly(ctx context.Context, a *Assemblers, marker jetstream.Msg) *assembly {
	traceID := marker.Subject()[strings.LastIndexByte(marker.Subject(), '.')+1:]
	return &assembly{ctx: ctx, a: a, marker: marker, traceID: traceID, subject: a.q.cfg.Spans.Subject + "." + traceID}
}

func (j *assembly) run() {
	last, err := j.a.q.spans.GetLastMsgForSubject(j.ctx, j.subject)
	switch {
	case errors.Is(err, jetstream.ErrMsgNotFound):
		j.done() // already assembled, or the spans aged out
		return
	case err != nil:
		j.fail("read spans", err)
		return
	}
	if wait := j.untilComplete(last.Time); wait > 0 {
		j.retryIn(wait)
		return
	}
	if op, err := j.assembleAndQueue(last.Sequence); err != nil {
		j.fail(op, err) // the spans stay put; the marker brings us back
		return
	}
	// Remove exactly the spans that were read; later arrivals stay.
	if err := j.a.q.spans.Purge(j.ctx, jetstream.WithPurgeSubject(j.subject), jetstream.WithPurgeSequence(last.Sequence+1)); err != nil {
		j.fail("purge spans", err)
		return
	}
	j.done()
	j.remarkLateSpans()
}

// untilComplete is zero once the trace has been quiet for IdleTimeout, or
// MaxWait has passed since its first span (the marker's timestamp).
func (j *assembly) untilComplete(lastSpan time.Time) time.Duration {
	sc, now := j.a.q.cfg.Spans, time.Now()
	quietFor := now.Sub(lastSpan)
	age := sc.MaxWait
	if meta, err := j.marker.Metadata(); err == nil {
		age = now.Sub(meta.Timestamp)
	}
	if quietFor >= sc.IdleTimeout || age >= sc.MaxWait {
		return 0
	}
	return max(min(sc.IdleTimeout-quietFor, sc.MaxWait-age), assemblerMinRecheck)
}

// Spans that make no scorable trace are discarded, which is not an error.
func (j *assembly) assembleAndQueue(lastSeq uint64) (fault.Op, error) {
	batches, err := j.a.readSpans(j.ctx, j.subject, lastSeq)
	if err != nil {
		return "read spans", err
	}
	logger := j.a.q.logger
	t, err := j.a.assemble(j.traceID, batches)
	if err != nil {
		logger.Info("spans discarded", "trace_id", j.traceID, "batches", len(batches), "reason", err.Error())
		return "", nil
	}
	if err := j.a.publish(j.ctx, t, lastSeq); err != nil {
		return "queue assembled trace, will retry", err
	}
	logger.Info("trace assembled from spans",
		"trace_id", j.traceID, "batches", len(batches), "tool_calls", len(t.ToolCalls), "evidence", len(t.Evidence))
	return "", nil
}

// Spans that arrived during this assembly found the old marker in place and
// could not leave their own, so leave one for them.
func (j *assembly) remarkLateSpans() {
	if _, err := j.a.q.spans.GetLastMsgForSubject(j.ctx, j.subject); err != nil {
		return
	}
	pending := j.a.q.cfg.Spans.PendingSubject + "." + j.traceID
	if _, err := j.a.q.js.Publish(j.ctx, pending, nil); err != nil && !common.IsSubjectFull(err) {
		j.report("re-mark trace", err)
	}
}

func (j *assembly) done() { j.a.ack(j.marker, j.traceID) }

func (j *assembly) fail(op fault.Op, err error) {
	j.report(op, err)
	j.retryIn(queue.AssemblerRetryDelay)
}

func (j *assembly) retryIn(delay time.Duration) {
	if err := j.marker.NakWithDelay(delay); err != nil {
		j.report("reschedule assembly", err)
	}
}

func (j *assembly) report(op fault.Op, err error) {
	j.a.q.report(fault.Unavailable(fault.Queue, op, err).WithTrace(j.traceID))
}

func (a *Assemblers) readSpans(ctx context.Context, subject string, upTo uint64) ([][]byte, error) {
	var batches [][]byte
	for seq := uint64(1); seq <= upTo; {
		msg, err := a.q.spans.GetMsg(ctx, seq, jetstream.WithGetMsgSubject(subject))
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		if msg.Sequence > upTo {
			break
		}
		batches = append(batches, msg.Data)
		seq = msg.Sequence + 1
	}
	return batches, nil
}

// The message ID makes a repeat of the same assembly (after a crash between
// queueing and purging) a no-op.
func (a *Assemblers) publish(ctx context.Context, t trace.Trace, lastSeq uint64) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if limit := a.q.conn.MaxPayload(); limit > 0 && len(data) > limit && len(t.OTLP) > 0 {
		// Too big to queue with its spans: score it anyway, and if kept export it as
		// rebuilt spans.
		a.q.report(fault.Invalid(fault.Queue, "queue assembled trace", errors.New("trace exceeds the NATS payload limit, queued without its original spans")).
			WithTrace(t.TraceID).With("bytes", len(data)).With("max_payload", limit))
		t.OTLP = nil
		if data, err = json.Marshal(t); err != nil {
			return err
		}
	}
	return a.q.work.Publish(ctx, t.TraceID, data, jetstream.WithMsgID(fmt.Sprintf("%s:%d", t.TraceID, lastSeq)))
}

func (a *Assemblers) ack(marker jetstream.Msg, traceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	if err := marker.DoubleAck(ctx); err != nil {
		a.q.report(fault.Unavailable(fault.Queue, "ack pending marker", err).AsSevere().WithTrace(traceID))
	}
}
