// Package fault is the engine's error type. Callers branch on its Kind, never
// on another package's sentinel errors or error strings.
package fault

import (
	"context"
	"errors"
	"fmt"
	"maps"
)

type Kind uint8

const (
	KindInternal    Kind = iota // a bug or an unexpected state
	KindInvalid                 // the input is wrong; retrying cannot help
	KindRejected                // a dependency refused this request; retrying cannot help
	KindUnavailable             // a dependency is down; retry later
	KindTimeout                 // a dependency did not answer in time; retry later
	KindFull                    // a bounded resource is at capacity; retry later
)

var kindNames = map[Kind]string{
	KindInternal: "internal", KindInvalid: "invalid", KindRejected: "rejected",
	KindUnavailable: "unavailable", KindTimeout: "timeout", KindFull: "full",
}

func (k Kind) String() string { return kindNames[k] }

type Component string

const (
	Config   Component = "config"
	Server   Component = "server"
	OTLP     Component = "otlp"
	Queue    Component = "queue"
	Pipeline Component = "pipeline"
	Scorer   Component = "scorer"
	Policy   Component = "policy"
	Export   Component = "export"
)

type Op string

type Error struct {
	Kind      Kind
	Component Component
	Op        Op
	TraceID   string
	// Severe marks lost or abandoned work, such as a dead letter; it is reported at
	// error level whatever its kind.
	Severe bool
	Attrs  map[string]any
	Cause  error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %s", e.Component, e.Op)
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) WithTrace(id string) *Error {
	out := *e
	out.TraceID = id
	return &out
}

func (e *Error) With(key string, value any) *Error {
	out := *e
	out.Attrs = maps.Clone(e.Attrs)
	if out.Attrs == nil {
		out.Attrs = map[string]any{}
	}
	out.Attrs[key] = value
	return &out
}

func (e *Error) AsSevere() *Error {
	out := *e
	out.Severe = true
	return &out
}

func New(kind Kind, component Component, op Op, cause error) *Error {
	return &Error{Kind: kind, Component: component, Op: op, Cause: cause}
}

func Internal(c Component, op Op, cause error) *Error    { return New(KindInternal, c, op, cause) }
func Invalid(c Component, op Op, cause error) *Error     { return New(KindInvalid, c, op, cause) }
func Rejected(c Component, op Op, cause error) *Error    { return New(KindRejected, c, op, cause) }
func Unavailable(c Component, op Op, cause error) *Error { return New(KindUnavailable, c, op, cause) }
func Timeout(c Component, op Op, cause error) *Error     { return New(KindTimeout, c, op, cause) }
func Full(c Component, op Op, cause error) *Error        { return New(KindFull, c, op, cause) }

// KindOf treats a context deadline as a timeout and any other foreign error as
// internal.
func KindOf(err error) Kind {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Kind
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout
	}
	return KindInternal
}

func Retryable(err error) bool {
	switch KindOf(err) {
	case KindUnavailable, KindTimeout, KindFull:
		return true
	}
	return false
}
