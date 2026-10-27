package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tecnosam/tailwise/internal/fault"
)

type counter struct {
	mu   sync.Mutex
	seen []string
}

func (c *counter) ErrorReported(_ context.Context, kind, component string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, component+"/"+kind)
}

func TestReportWritesOneRecordAndCountsIt(t *testing.T) {
	var out bytes.Buffer
	c := &counter{}
	p := New(slog.NewJSONHandler(&out, nil), c)

	p.Report(context.Background(), fault.Unavailable(fault.Queue, "nak trace", errors.New("nats: timeout")).WithTrace("t1").With("delivery", 2))

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not one JSON line: %q", out.String())
	}
	want := map[string]any{"level": "WARN", "msg": "nak trace", "kind": "unavailable",
		"component": "queue", "trace_id": "t1", "error": "nats: timeout", "delivery": float64(2)}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["time"]; !ok {
		t.Error("record has no time")
	}
	if len(c.seen) != 1 || c.seen[0] != "queue/unavailable" {
		t.Errorf("counted %v", c.seen)
	}
}

func TestLevels(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want string
	}{
		{fault.Rejected(fault.Scorer, "score", nil), "WARN"},
		{fault.Unavailable(fault.Queue, "publish", nil).AsSevere(), "ERROR"},
		{fault.Internal(fault.Pipeline, "decide", nil), "ERROR"},
		{errors.New("not ours"), "ERROR"},
	} {
		var out bytes.Buffer
		New(slog.NewJSONHandler(&out, nil), nil).Report(context.Background(), tt.err)
		if !strings.Contains(out.String(), `"level":"`+tt.want+`"`) {
			t.Errorf("%v: %s, want level %s", tt.err, out.String(), tt.want)
		}
	}
}

// failing is a sink that cannot write.
type failing struct{ slog.Handler }

func (failing) Handle(context.Context, slog.Record) error { return errors.New("disk full") }

// A broken sink does not stop the others, and the record is still counted.
func TestABrokenSinkDoesNotStopTheOthers(t *testing.T) {
	var out bytes.Buffer
	c := &counter{}
	good := slog.NewJSONHandler(&out, nil)
	p := New(slog.NewMultiHandler(failing{good}, good), c)
	p.Report(context.Background(), fault.Invalid(fault.OTLP, "store spans", nil))
	if !strings.Contains(out.String(), "store spans") || len(c.seen) != 1 {
		t.Errorf("out %q, counted %v", out.String(), c.seen)
	}
}

func TestFileSinkAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errors.jsonl")
	p, err := Open(context.Background(), []SinkConfig{{Kind: SinkFile, Path: path}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Report(context.Background(), fault.Invalid(fault.OTLP, "store spans", nil))
	p.Report(context.Background(), fault.Invalid(fault.OTLP, "store spans", nil))
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Errorf("%d lines, want 2: %s", n, data)
	}
}

func TestUnknownSinkIsAConfigError(t *testing.T) {
	_, err := Open(context.Background(), []SinkConfig{{Kind: "datadog"}}, nil)
	if fault.KindOf(err) != fault.KindInvalid {
		t.Errorf("error = %v", err)
	}
}

// The OTLP sink posts each error to a collector as an OTLP log record, with
// its headers, by Close at the latest.
func TestOTLPSinkExportsErrors(t *testing.T) {
	var mu sync.Mutex
	var path, auth string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer srv.Close()

	p, err := Open(context.Background(), []SinkConfig{{
		Kind: SinkOTLP, Endpoint: srv.URL + "/v1/logs", Headers: map[string]string{"Authorization": "Bearer t"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Report(context.Background(), fault.Unavailable(fault.Scorer, "score trace", errors.New("jev 503")).WithTrace("t9"))
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if path != "/v1/logs" || auth != "Bearer t" {
		t.Errorf("request to %q with Authorization %q", path, auth)
	}
	// Protobuf keeps strings as plain bytes.
	for _, want := range []string{"score trace", "jev 503", "t9", "scorer", "unavailable", "tailwise"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("OTLP payload does not contain %q", want)
		}
	}
}
