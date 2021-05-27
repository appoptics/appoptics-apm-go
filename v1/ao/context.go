// Copyright (C) 2016 Librato, Inc. All rights reserved.

package ao

import (
	"context"
	"github.com/appoptics/appoptics-apm-go/v1/ao/internal/log"
	"github.com/appoptics/appoptics-apm-go/v1/ao/internal/reporter"
)

type contextKeyT interface{}

var contextKey = contextKeyT("github.com/appoptics/appoptics-apm-go/v1/ao.Trace")
var contextSpanKey = contextKeyT("github.com/appoptics/appoptics-apm-go/v1/ao.Span")

// NewContext returns a copy of the parent context and associates it with a Trace.
func NewContext(ctx context.Context, t Trace) context.Context {
	return context.WithValue(context.WithValue(ctx, contextKey, t), contextSpanKey, t)
}

// newSpanContext returns a copy of the parent context and associates it with a Span.
func newSpanContext(ctx context.Context, l Span) context.Context {
	return context.WithValue(ctx, contextSpanKey, l)
}

func FromXTraceIDContext(ctx context.Context, xTraceID string) context.Context {
	aoCtx, err := reporter.NewContextFromMetadataString(xTraceID)
	if err != nil {
		log.Warningf("xTrace ID %v is invalid \n", xTraceID)
	}
	return context.WithValue(ctx, contextSpanKey, contextSpan{aoCtx: aoCtx})
}

// FromContext returns the Span bound to the context, if any.
func FromContext(ctx context.Context) Span {
	l, ok := fromContext(ctx)
	if !ok {
		return nullSpan{}
	}
	return l
}
func fromContext(ctx context.Context) (l Span, ok bool) {
	if ctx == nil {
		return nil, false
	}
	l, ok = ctx.Value(contextSpanKey).(Span)
	return
}

// TraceFromContext returns the Trace bound to the context, if any.
func TraceFromContext(ctx context.Context) Trace {
	t, ok := traceFromContext(ctx)
	if !ok {
		return &nullTrace{}
	}
	return t
}
func traceFromContext(ctx context.Context) (t Trace, ok bool) {
	if ctx == nil {
		return nil, false
	}
	t, ok = ctx.Value(contextKey).(Trace)
	return
}

// if context contains a valid Span, run f
func runCtx(ctx context.Context, f func(l Span)) {
	if l, ok := fromContext(ctx); ok {
		f(l)
	}
}

// if context contains a valid Trace, run f
func runTraceCtx(ctx context.Context, f func(t Trace)) {
	if t, ok := traceFromContext(ctx); ok {
		f(t)
	}
}

// EndTrace ends a Trace, given a context that was associated with the trace.
func EndTrace(ctx context.Context) { runTraceCtx(ctx, func(t Trace) { t.End() }) }

// End ends a Span, given a context ctx that was associated with it, optionally reporting KV pairs
// provided by args.
func End(ctx context.Context, args ...interface{}) { runCtx(ctx, func(l Span) { l.End(args...) }) }

// Info reports KV pairs provided by args for the Span associated with the context ctx.
func Info(ctx context.Context, args ...interface{}) { runCtx(ctx, func(l Span) { l.Info(args...) }) }

// Error reports details about an error (along with a stack trace) on the Span associated with the context ctx.
func Error(ctx context.Context, class, msg string) { runCtx(ctx, func(l Span) { l.Error(class, msg) }) }

// Err reports details error err (along with a stack trace) on the Span associated with the context ctx.
func Err(ctx context.Context, err error) { runCtx(ctx, func(l Span) { l.Err(err) }) }

// MetadataString returns a representation of the Span's context for use with distributed
// tracing (to create a remote child span). If the Span has ended, an empty string is returned.
func MetadataString(ctx context.Context) string {
	if l, ok := fromContext(ctx); ok {
		return l.MetadataString()
	}
	return ""
}

// IsSampled returns whether or not the Layer span's context is sampled
func IsSampled(ctx context.Context) bool {
	if l, ok := fromContext(ctx); ok {
		return l.IsSampled()
	}
	return false
}
