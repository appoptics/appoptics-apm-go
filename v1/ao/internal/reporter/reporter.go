// Copyright (C) 2017 Librato, Inc. All rights reserved.

package reporter

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"strings"
	"time"

	"github.com/appoptics/appoptics-apm-go/v1/ao/internal/config"
	"github.com/appoptics/appoptics-apm-go/v1/ao/internal/host"
	"github.com/appoptics/appoptics-apm-go/v1/ao/internal/log"
	"github.com/appoptics/appoptics-apm-go/v1/ao/internal/metrics"
)

// defines what methods a reporter should offer (internal to reporter package)
type reporter interface {
	// called when an event should be reported
	reportEvent(ctx *oboeContext, e *event) error
	// called when a status (e.g. __Init message) should be reported
	reportStatus(ctx *oboeContext, e *event) error
	// called when a Span message should be reported
	reportSpan(span metrics.SpanMessage) error
	// Shutdown closes the reporter.
	Shutdown(ctx context.Context) error
	// ShutdownNow closes the reporter immediately
	ShutdownNow() error
	// Closed returns if the reporter is already closed.
	Closed() bool
	// WaitForReady waits until the reporter becomes ready or the context is canceled.
	WaitForReady(context.Context) bool
	// CustomSummaryMetric submits a summary type measurement to the reporter. The measurements
	// will be collected in the background and reported periodically.
	CustomSummaryMetric(name string, value float64, opts metrics.MetricOptions) error
	// CustomIncrementMetric submits a incremental measurement to the reporter. The measurements
	// will be collected in the background and reported periodically.
	CustomIncrementMetric(name string, opts metrics.MetricOptions) error
	// Flush flush the events buffer to stderr. Currently it's used for AWS Lambda only
	Flush() error
	// SetServiceKey attaches a service key to the reporter
	SetServiceKey(key string)
}

// KVs from getSettingsResult arguments
const (
	kvSignatureKey                      = "SignatureKey"
	kvBucketCapacity                    = "BucketCapacity"
	kvBucketRate                        = "BucketRate"
	kvTriggerTraceRelaxedBucketCapacity = "TriggerRelaxedBucketCapacity"
	kvTriggerTraceRelaxedBucketRate     = "TriggerRelaxedBucketRate"
	kvTriggerTraceStrictBucketCapacity  = "TriggerStrictBucketCapacity"
	kvTriggerTraceStrictBucketRate      = "TriggerStrictBucketRate"
	kvMetricsFlushInterval              = "MetricsFlushInterval"
	kvEventsFlushInterval               = "EventsFlushInterval"
	kvMaxTransactions                   = "MaxTransactions"
	kvMaxCustomMetrics                  = "MaxCustomMetrics"
)

// currently used reporter
var globalReporter reporter = &nullReporter{}

var (
	periodicTasksDisabled = false // disable periodic tasks, for testing
)

// a noop reporter
type nullReporter struct{}

func newNullReporter() *nullReporter                                  { return &nullReporter{} }
func (r *nullReporter) reportEvent(ctx *oboeContext, e *event) error  { return nil }
func (r *nullReporter) reportStatus(ctx *oboeContext, e *event) error { return nil }
func (r *nullReporter) reportSpan(span metrics.SpanMessage) error     { return nil }
func (r *nullReporter) Shutdown(ctx context.Context) error            { return nil }
func (r *nullReporter) ShutdownNow() error                            { return nil }
func (r *nullReporter) Closed() bool                                  { return true }
func (r *nullReporter) WaitForReady(ctx context.Context) bool         { return true }
func (r *nullReporter) CustomSummaryMetric(name string, value float64, opts metrics.MetricOptions) error {
	return nil
}
func (r *nullReporter) CustomIncrementMetric(name string, opts metrics.MetricOptions) error {
	return nil
}
func (r *nullReporter) Flush() error { return nil }
func (r *nullReporter) SetServiceKey(string) {}

// init() is called only once on program startup. Here we create the reporter
// that will be used throughout the runtime of the app. Default is 'ssl' but
// can be overridden via APPOPTICS_REPORTER
func init() {
	log.SetLevelFromStr(config.DebugLevel())
	initReporter()
	sendInitMessage()
}

func initReporter() {
	var rt string
	if config.GetDisabled() {
		log.Warning("AppOptics APM agent is disabled.")
		rt = "none"
	} else {
		rt = config.GetReporterType()
	}
	setGlobalReporter(rt)
}

func setGlobalReporter(reporterType string) {
	// Close the previous reporter
	if globalReporter != nil {
		globalReporter.ShutdownNow()
	}

	switch strings.ToLower(reporterType) {
	case "ssl":
		fallthrough // using fallthrough since the SSL reporter (gRPC) is our default reporter
	default:
		globalReporter = newGRPCReporter()
	case "udp":
		globalReporter = udpNewReporter()
	case "none":
		globalReporter = newNullReporter()
	case "serverless":
		globalReporter = newServerlessReporter(os.Stderr)
	}
}

// WaitForReady waits until the reporter becomes ready or the context is canceled.
func WaitForReady(ctx context.Context) bool {
	// globalReporter is not protected by a mutex as currently it's only modified
	// from the init() function.
	return globalReporter.WaitForReady(ctx)
}

// Flush flush the events buffer to stderr. Currently it's used for AWS Lambda only
func Flush() error {
	return globalReporter.Flush()
}

// Shutdown flushes the metrics and stops the reporter. It blocked until the reporter
// is shutdown or the context is canceled.
func Shutdown(ctx context.Context) error {
	return globalReporter.Shutdown(ctx)
}

// Closed indicates if the reporter has been shutdown
func Closed() bool {
	return globalReporter.Closed()
}

// ReportSpan is called from the app when a span message is available
// span	span message to be put on the channel
//
// returns	error if channel is full
func ReportSpan(span metrics.SpanMessage) error {
	return globalReporter.reportSpan(span)
}

// check if context and event are valid, add general keys like Timestamp, or hostname
// ctx		oboe context
// e		event to be prepared for sending
//
// returns	error if invalid context or event
func prepareEvent(ctx *oboeContext, e *event) error {
	if ctx == nil || e == nil {
		return errors.New("invalid context, event")
	}

	// For now, disable the check if the entry event is using an explicit x-trace ID
	// as the current logic in context.NewContext would use the same x-trace ID for
	// context as the entry event. Therefore the opID is the same.
	if e.overrides.ExplicitMdStr == "" {
		// The context metadata must have the same task_id as the event.
		if !bytes.Equal(ctx.metadata.ids.taskID, e.metadata.ids.taskID) {
			return errors.New("invalid event, different task_id from context")
		}

		// The context metadata must have a different op_id than the event.
		if bytes.Equal(ctx.metadata.ids.opID, e.metadata.ids.opID) {
			return errors.New("invalid event, same as context")
		}
	}

	var us int64
	if e.overrides.ExplicitTS.IsZero() {
		us = time.Now().UnixNano() / 1000
	} else {
		us = e.overrides.ExplicitTS.UnixNano() / 1000
	}

	e.AddInt64("Timestamp_u", us)

	e.AddString("Hostname", host.Hostname())
	e.AddInt("PID", host.PID())

	// Update the context's op_id to that of the event
	ctx.metadata.ids.setOpID(e.metadata.ids.opID)

	e.bbuf.Finish()
	return nil
}

func shouldTraceRequestWithURL(layer string, traced bool, url string, triggerTrace TriggerTraceMode) SampleDecision {
	return oboeSampleRequest(layer, traced, url, triggerTrace)
}

// Determines if request should be traced, based on sample rate settings.
func shouldTraceRequest(layer string, traced bool) (bool, int, sampleSource, bool) {
	d := shouldTraceRequestWithURL(layer, traced, "", ModeTriggerTraceNotPresent)
	return d.trace, d.rate, d.source, d.enabled
}

func argsToMap(capacity, ratePerSec, tRCap, tRRate, tSCap, tSRate float64,
	metricsFlushInterval, maxTransactions int, token []byte) map[string][]byte {
	args := make(map[string][]byte)

	if capacity > -1 {
		bits := math.Float64bits(capacity)
		bytes := make([]byte, 8)
		binary.LittleEndian.PutUint64(bytes, bits)
		args[kvBucketCapacity] = bytes
	}
	if ratePerSec > -1 {
		bits := math.Float64bits(ratePerSec)
		bytes := make([]byte, 8)
		binary.LittleEndian.PutUint64(bytes, bits)
		args[kvBucketRate] = bytes
	}
	if tRCap > -1 {
		bits := math.Float64bits(tRCap)
		bytes := make([]byte, 8)
		binary.LittleEndian.PutUint64(bytes, bits)
		args[kvTriggerTraceRelaxedBucketCapacity] = bytes
	}
	if tRRate > -1 {
		bits := math.Float64bits(tRRate)
		bytes := make([]byte, 8)
		binary.LittleEndian.PutUint64(bytes, bits)
		args[kvTriggerTraceRelaxedBucketRate] = bytes
	}
	if tSCap > -1 {
		bits := math.Float64bits(tSCap)
		bytes := make([]byte, 8)
		binary.LittleEndian.PutUint64(bytes, bits)
		args[kvTriggerTraceStrictBucketCapacity] = bytes
	}
	if tSRate > -1 {
		bits := math.Float64bits(tSRate)
		bytes := make([]byte, 8)
		binary.LittleEndian.PutUint64(bytes, bits)
		args[kvTriggerTraceStrictBucketRate] = bytes
	}
	if metricsFlushInterval > -1 {
		bytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(bytes, uint32(metricsFlushInterval))
		args[kvMetricsFlushInterval] = bytes
	}
	if maxTransactions > -1 {
		bytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(bytes, uint32(maxTransactions))
		args[kvMaxTransactions] = bytes
	}

	args[kvSignatureKey] = token

	return args
}

// SummaryMetric submits a summary type measurement to the reporter. The measurements
// will be collected in the background and reported periodically.
func SummaryMetric(name string, value float64, opts metrics.MetricOptions) error {
	return globalReporter.CustomSummaryMetric(name, value, opts)
}

// IncrementMetric submits a incremental measurement to the reporter. The measurements
// will be collected in the background and reported periodically.
func IncrementMetric(name string, opts metrics.MetricOptions) error {
	return globalReporter.CustomIncrementMetric(name, opts)
}

func SetServiceKey(key string) {
	globalReporter.SetServiceKey(key)
}