package opentelemetry

import (
	"context"
	"sync"
	"time"

	"github.com/appoptics/appoptics-apm-go/v1/ao"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/attribute"

	"go.opentelemetry.io/otel/codes"
)

// TODO test
type spanImpl struct {
	mu            sync.Mutex
	tracer        trace.Tracer
	aoSpan        ao.Span
	context       trace.SpanContext
	name          string
	statusCode    codes.Code
	statusMessage string
	attributes    []attribute.KeyValue
	links         []trace.Link
	parent        trace.Span
}

var _ trace.Span = (*spanImpl)(nil)

func Wrapper(aoSpan ao.Span) trace.Span {
	return &spanImpl{
		tracer:  nil, // TODO no tracerImpl for it, should be OK?
		aoSpan:  aoSpan,
		context: MdStr2OTSpanContext(aoSpan.MetadataString()),
		name:    "", // TODO expose AO span name
	}
}

func (s *spanImpl) Tracer() trace.Tracer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracer
}

func (s *spanImpl) End(options ...trace.SpanOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var args []interface{}
	for _, link := range s.links {
		args = append(args, "link", OTSpanContext2MdStr(link.SpanContext))
	}
	for _, attr := range s.attributes {
		args = append(args, string(attr.Key), attr.Value.AsInterface())
	}

	cfg := trace.NewSpanConfig(options...)
	if !cfg.Timestamp.IsZero() {
		args = append(args, "Timestamp_u", cfg.Timestamp.UnixNano()/1000)
	}
	s.aoSpan.End(args...)
}

func (s *spanImpl) 	AddEvent(name string, options ...trace.EventOption) {
	// TODO
}

// func (s *spanImpl) AddEvent(ctx context.Context, name string, attrs ...core.KeyValue) {
// 	s.mu.Lock()
// 	defer s.mu.Unlock()
// 	s.addEventWithTimestamp(ctx, time.Now(), name, attrs...)
// }

func (s *spanImpl) addEventWithTimestamp(ctx context.Context, timestamp time.Time,
	name string, attrs ...attribute.KeyValue) {
	var args []interface{}
	args = append(args, "Name", name)
	for _, attr := range attrs {
		args = append(args, string(attr.Key), attr.Value.AsInterface())
	}
	if !timestamp.IsZero() {
		args = append(args, "Timestamp_u", timestamp.UnixNano()/1000)
	}
	s.aoSpan.Info(args...)
}
//
// func (s *spanImpl) AddEventWithTimestamp(ctx context.Context, timestamp time.Time,
// 	name string, attrs ...core.KeyValue) {
// 	s.mu.Lock()
// 	defer s.mu.Unlock()
// 	s.addEventWithTimestamp(ctx, timestamp, name, attrs...)
// }

func (s *spanImpl) IsRecording() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aoSpan.IsReporting()
}

func (s *spanImpl) 	RecordError(err error, options ...trace.EventOption) {
	// TODO
}

// func (s *spanImpl) RecordError(ctx context.Context, err error, opts ...trace.ErrorOption) {
// 	// Do not lock s.mu here otherwise there will be a deadlock as it calls s.SetStatus
// 	if err == nil {
// 		return
// 	}
//
// 	cfg := &trace.ErrorConfig{}
// 	for _, opt := range opts {
// 		opt(cfg)
// 	}
//
// 	if cfg.Timestamp.IsZero() {
// 		cfg.Timestamp = time.Now()
// 	}
// 	if cfg.StatusCode != codes.OK {
// 		s.SetStatus(cfg.StatusCode, "")
// 	}
// 	s.aoSpan.ErrWithOptions(ao.ErrorOptions{
// 		Timestamp: cfg.Timestamp,
// 		Class:     fmt.Sprintf("error-%d", cfg.StatusCode),
// 		Msg:       err.Error(),
// 	})
// }

func (s *spanImpl) SpanContext() trace.SpanContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.context
}

func (s *spanImpl) SetStatus(code codes.Code, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusCode = code
	s.statusMessage = msg
}

func (s *spanImpl) SetName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
}

func (s *spanImpl) SetAttributes(attrs ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attributes = append(s.attributes, attrs...)
}

func (s *spanImpl) SetAttribute(k string, v interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attributes = append(s.attributes, attribute.Any(k, v))
}

func (s *spanImpl) addLink(link trace.Link) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.links = append(s.links, link)
}
