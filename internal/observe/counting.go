package observe

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

const (
	attrKind      = "kind"
	attrComponent = "component"
)

// counting counts each warning or error by kind and component as it passes the
// record on, so the metric and the log never disagree. A failing sink is noted
// on stderr.
type counting struct {
	slog.Handler
	counter ErrorCounter
}

func (h counting) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		var kind, component string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case attrKind:
				kind = a.Value.String()
			case attrComponent:
				component = a.Value.String()
			}
			return true
		})
		h.counter.ErrorReported(ctx, kind, component)
	}
	err := h.Handler.Handle(ctx, r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "observe: error sink failed: %v (entry: %s)\n", err, r.Message)
	}
	return err
}

func (h counting) WithAttrs(attrs []slog.Attr) slog.Handler {
	return counting{Handler: h.Handler.WithAttrs(attrs), counter: h.counter}
}

func (h counting) WithGroup(name string) slog.Handler {
	return counting{Handler: h.Handler.WithGroup(name), counter: h.counter}
}
