// Package server is the engine's HTTP API: queued intake, synchronous scoring
// and health checks.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tecnosam/tailwise/internal/common"
	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/metrics"
	"github.com/tecnosam/tailwise/internal/observe"
	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/trace"
)

const (
	defaultMaxBodyBytes = 8 << 20
	readyTimeout        = 2 * time.Second
	retryAfterSeconds   = "1"
)

type Options struct {
	Engine   *pipeline.Engine
	Queue    pipeline.Publisher
	Reporter observe.Reporter
	Metrics  common.RequestMetrics
	// MaxBodyBytes 0 means 8 MiB.
	MaxBodyBytes int64
}

type Server struct {
	opts Options
	mux  *http.ServeMux
}

func New(opts Options) *Server {
	if opts.Reporter == nil {
		opts.Reporter = observe.Discard
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBodyBytes
	}
	if opts.Metrics == nil {
		opts.Metrics = metrics.Noop{}
	}
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /v1/traces", s.handleEnqueue)
	s.mux.HandleFunc("POST /v1/traces:score", s.handleScore)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /openapi.yaml", s.handleOpenAPI)
	s.mux.HandleFunc("GET /docs", s.handleDocs)
	return s
}

func (s *Server) Handler() http.Handler { return common.InstrumentHTTP("api", s.mux, s.opts.Metrics) }

// 202 only after the queue has confirmed the trace, so it survives a restart.
func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	t, ok := s.readTrace(w, r)
	if !ok {
		return
	}
	if err := s.opts.Queue.Publish(r.Context(), t); err != nil {
		s.opts.Reporter.Report(r.Context(), fault.New(fault.KindOf(err), fault.Server, "enqueue trace, rejected it", err).WithTrace(t.TraceID))
		writeFault(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"trace_id": t.TraceID})
}

func (s *Server) handleScore(w http.ResponseWriter, r *http.Request) {
	t, ok := s.readTrace(w, r)
	if !ok {
		return
	}
	// The response does not wait for the export: the export workers send the
	// trace moments later.
	writeJSON(w, http.StatusOK, resultOf(s.opts.Engine.Process(r.Context(), t)))
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if problem := s.notReady(ctx); problem != "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": problem})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) notReady(ctx context.Context) string {
	if err := s.opts.Queue.Ready(); err != nil {
		return "queue is not reachable: " + err.Error()
	}
	if err := s.opts.Engine.Ready(ctx); err != nil {
		return "model is not reachable: " + err.Error()
	}
	return ""
}

// readTrace writes the error response itself on failure.
func (s *Server) readTrace(w http.ResponseWriter, r *http.Request) (trace.Trace, bool) {
	t, err := trace.Decode(http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes))
	if err != nil {
		writeDecodeError(w, err)
		return trace.Trace{}, false
	}
	if t.TraceID == "" {
		t.TraceID = trace.NewID()
	}
	t.OTLP = nil // spans come in through the OTLP receivers, not this API
	return t, true
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

// Anything worth retrying gets 503 with Retry-After.
func writeFault(w http.ResponseWriter, err error) {
	kind := fault.KindOf(err)
	switch {
	case kind == fault.KindFull:
		w.Header().Set("Retry-After", retryAfterSeconds)
		writeError(w, http.StatusServiceUnavailable, "queue full, retry later")
	case fault.Retryable(err):
		w.Header().Set("Retry-After", retryAfterSeconds)
		writeError(w, http.StatusServiceUnavailable, "queue unavailable, retry later")
	case kind == fault.KindInvalid:
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
