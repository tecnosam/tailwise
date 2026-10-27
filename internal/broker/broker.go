// Package broker opens the JetStream connection every queue lives on: the
// server at queue.url, or one embedded in the process, without a port, when it
// is empty. Both run the same queue code.
package broker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/tecnosam/tailwise/internal/common"
	"github.com/tecnosam/tailwise/internal/export"
	exportnats "github.com/tecnosam/tailwise/internal/export/natsq"
	"github.com/tecnosam/tailwise/internal/observe"
	"github.com/tecnosam/tailwise/internal/queue"
	tracenats "github.com/tecnosam/tailwise/internal/queue/natsq"
)

type Kind string

const (
	KindNATS     Kind = "nats"
	KindEmbedded Kind = "embedded"
)

const (
	// Matches nats.conf: an assembled trace travels as one message with all its
	// spans.
	embeddedMaxPayload = 8 << 20
	embeddedReady      = 10 * time.Second
)

type Config struct {
	// URL empty starts a NATS server embedded in the process.
	URL string
	// StoreDir empty means a temporary directory removed on Close, so queued work
	// is lost when the engine stops.
	StoreDir string
	// Replicas above the cluster's server count fails at start-up.
	Replicas int
	Auth     NATSAuth
	TLS      NATSTLS
	Logger   *slog.Logger
	Reporter observe.Reporter
}

type Broker struct {
	conn    *common.Conn
	srv     *server.Server // nil for an external server
	tempDir string         // removed on Close
}

func New(cfg Config) (*Broker, error) {
	if cfg.URL == "" {
		return newEmbedded(cfg)
	}
	opts, err := natsOptions(cfg.Auth, cfg.TLS)
	if err != nil {
		return nil, fmt.Errorf("nats: %w", err)
	}
	conn, err := common.DialNATS(common.NATSConfig{URL: cfg.URL, Logger: cfg.Logger, Reporter: cfg.Reporter, Options: opts})
	if err != nil {
		return nil, err
	}
	conn.Replicas = cfg.Replicas
	return &Broker{conn: conn}, nil
}

func newEmbedded(cfg Config) (*Broker, error) {
	b := &Broker{}
	dir := cfg.StoreDir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "tailwise-nats-")
		if err != nil {
			return nil, fmt.Errorf("embedded nats: %w", err)
		}
		dir, b.tempDir = tmp, tmp
	}
	srv, err := server.NewServer(&server.Options{
		ServerName: "tailwise-embedded",
		DontListen: true, // clients connect in memory only
		JetStream:  true,
		StoreDir:   dir,
		MaxPayload: embeddedMaxPayload,
		NoLog:      true,
		NoSigs:     true,
	})
	if err != nil {
		b.Close()
		return nil, fmt.Errorf("embedded nats: %w", err)
	}
	b.srv = srv
	go srv.Start()
	if !srv.ReadyForConnections(embeddedReady) {
		b.Close()
		return nil, fmt.Errorf("embedded nats: not ready after %s", embeddedReady)
	}
	conn, err := common.DialNATS(common.NATSConfig{
		Logger: cfg.Logger, Reporter: cfg.Reporter, Options: []nats.Option{nats.InProcessServer(srv)},
	})
	if err != nil {
		b.Close()
		return nil, err
	}
	b.conn = conn
	return b, nil
}

func (b *Broker) Kind() Kind {
	if b.srv != nil {
		return KindEmbedded
	}
	return KindNATS
}

// Ephemeral reports whether queued work is lost when the engine stops.
func (b *Broker) Ephemeral() bool { return b.tempDir != "" }

func (b *Broker) Ingest(ctx context.Context, cfg queue.Config) (queue.AbstractBroker, error) {
	return tracenats.New(ctx, b.conn, cfg)
}

func (b *Broker) ExportQueues(streams exportnats.Streams) export.QueueFunc {
	return exportnats.Queues(b.conn, streams)
}

// Close what was built on the broker first.
func (b *Broker) Close() {
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
	if b.srv != nil {
		b.srv.Shutdown()
		b.srv.WaitForShutdown()
		b.srv = nil
	}
	if b.tempDir != "" {
		_ = os.RemoveAll(b.tempDir)
		b.tempDir = ""
	}
}
