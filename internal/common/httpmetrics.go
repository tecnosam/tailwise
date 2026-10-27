package common

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/tecnosam/tailwise/internal/metrics"
)

type RequestMetrics interface {
	RequestFinished(ctx context.Context, req metrics.Request)
}

func InstrumentHTTP(server string, h http.Handler, m RequestMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		m.RequestFinished(r.Context(), metrics.Request{
			Server:   server,
			Protocol: metrics.ProtocolHTTP,
			Method:   r.Method,
			Route:    routeOf(r),
			Status:   rec.status,
			Duration: time.Since(start),
		})
	})
}

// routeOf is the pattern the ServeMux matched, so metrics are not split by
// raw path.
func routeOf(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}
	_, path, found := strings.Cut(r.Pattern, " ")
	if !found {
		return r.Pattern
	}
	return path
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
