package broker

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/tecnosam/tailwise/internal/export"
	exportnats "github.com/tecnosam/tailwise/internal/export/natsq"
	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/queue"
	"github.com/tecnosam/tailwise/internal/queue/queuetest"
	"github.com/tecnosam/tailwise/internal/retry"
	"github.com/tecnosam/tailwise/internal/trace"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func queueConfig() queue.Config {
	return queue.Config{
		ConnectTimeout: 5 * time.Second,
		Stream:         "TRACES", Subject: "traces.in", Consumer: "workers",
		MaxMessages: 10, AckWait: 30 * time.Second, Redelivery: retry.Policy{Attempts: 2},
		DLQStream: "TRACES_DLQ", DLQSubject: "traces.dlq", DLQMaxAge: time.Hour,
		PollWait: 100 * time.Millisecond, Logger: quiet,
	}
}

var streams = exportnats.Streams{
	Stream: "EXPORTS", Subject: "exports", Consumer: "workers-export",
	DLQStream: "EXPORTS_DLQ", DLQSubject: "exports_dlq", DLQMaxAge: time.Hour, PollWait: 100 * time.Millisecond,
}

func TestNewChoosesByURL(t *testing.T) {
	f, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Kind() != KindEmbedded {
		t.Errorf("no URL: kind %s, want embedded", f.Kind())
	}

	srv := queuetest.StartServer(t)
	f, err = New(Config{URL: srv.ClientURL(), Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Kind() != KindNATS {
		t.Errorf("with URL: kind %s, want nats", f.Kind())
	}
}

// Each family builds a working trace queue and export queue.
func TestFamiliesWork(t *testing.T) {
	srv := queuetest.StartServer(t)
	for _, url := range []string{"", srv.ClientURL()} {
		f, err := New(Config{URL: url, Logger: quiet})
		if err != nil {
			t.Fatal(err)
		}
		t.Run(string(f.Kind()), func(t *testing.T) {
			defer f.Close()
			q, err := f.Ingest(context.Background(), queueConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			got := make(chan string, 1)
			w := q.StartWorkers(1, func(_ context.Context, tr trace.Trace) error {
				got <- tr.TraceID
				return nil
			})
			defer w.Stop(context.Background())
			if err := q.Publish(context.Background(), trace.Trace{TraceID: "t1", Question: "q", FinalAnswer: "a"}); err != nil {
				t.Fatal(err)
			}
			wantReceived(t, got, "t1")

			sent := make(chan string, 1)
			eq, err := f.ExportQueues(streams)(context.Background(), export.QueueConfig{
				Name: "phoenix",
				Send: func(_ context.Context, rec pipeline.Record) error {
					sent <- rec.Trace.TraceID
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer eq.Shutdown(context.Background())
			if err := eq.Enqueue(context.Background(), pipeline.Record{Trace: trace.Trace{TraceID: "t2"}}); err != nil {
				t.Fatal(err)
			}
			wantReceived(t, sent, "t2")
		})
	}
}

func wantReceived(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Errorf("received %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%q never arrived", want)
	}
}

// Without a store_dir the embedded server keeps its streams in a temporary
// directory, removed on Close.
func TestEmbeddedTempDirIsRemoved(t *testing.T) {
	b, err := New(Config{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	dir := b.tempDir
	if !b.Ephemeral() || dir == "" {
		t.Fatalf("Ephemeral = %v, temp dir %q; want a temporary store", b.Ephemeral(), dir)
	}
	q, err := b.Ingest(context.Background(), queueConfig())
	if err != nil {
		t.Fatal(err)
	}
	q.Close()
	b.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir %s still exists after Close (err %v)", dir, err)
	}
}

// With a store_dir, a trace queued before a restart is still there after it.
func TestEmbeddedStoreDirSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	open := func() (*Broker, queue.AbstractBroker) {
		b, err := New(Config{StoreDir: dir, Logger: quiet})
		if err != nil {
			t.Fatal(err)
		}
		if b.Ephemeral() {
			t.Fatal("a broker with a store_dir reports itself ephemeral")
		}
		q, err := b.Ingest(context.Background(), queueConfig())
		if err != nil {
			t.Fatal(err)
		}
		return b, q
	}

	b, q := open()
	if err := q.Publish(context.Background(), trace.Trace{TraceID: "kept", Question: "q", FinalAnswer: "a"}); err != nil {
		t.Fatal(err)
	}
	q.Close()
	b.Close()

	b, q = open()
	defer b.Close()
	defer q.Close()
	got := make(chan string, 1)
	w := q.StartWorkers(1, func(_ context.Context, tr trace.Trace) error {
		got <- tr.TraceID
		return nil
	})
	defer w.Stop(context.Background())
	wantReceived(t, got, "kept")
}
