// Copyright (C) 2017 Librato, Inc. All rights reserved.

package traceview

import (
	"time"
	"strconv"
)

const (
	MetricsRecordMaxSize = 100
	MaxTransactionNames = 200
	DefaultHistogramPrecision = 2
)

// MetricsAggregator processes the metrics records and calculate the metrics and
// histograms message from them.
type MetricsAggregator interface {
	// FlushBSON requests a metrics message from the message channel and encode
	// it into a BSON message.
	FlushBSON() [][]byte
	// ProcessMetrics consumes the metrics records from the records channel
	// and update the histograms and metrics based on the records. It will
	// send a metrics message to the message channel on request and may reset
	// the histograms and metrics. It is started as a separate goroutine.
	ProcessMetrics()
	// PushMetricsRecord push a metrics record to the records channel and return
	// immediately if the channel is full.
	PushMetricsRecord(record MetricsRecord) bool
}

type metricsAggregator struct {
	// Receive the MetricsRecord sent by traces and update the MetricsRaw.
	records chan MetricsRecord
	// Receive a request from periodic goroutine for a new MetricsRaw struct
	rawReq chan struct{}
	// Send the MetricsRaw through this channel to periodic goroutine
	raw chan MetricsRaw
	// Used to notify the ProcessMetrics goroutine to exit
	exit chan struct{}
	// Stores the seen transaction names, the limit is defined by MaxTransactionNames
	transNames map[string]bool
	// The raw struct of histograms and measurements, it's consumed by FlushBSON to create
	// the metrics message
	metrics MetricsRaw

	// system metadata cache (expensive to get)
	cachedSysMeta map[string]string
}

type MetricsRaw struct {
	histograms map[string]*Histogram
	measurements map[string]*Measurement
}

type baseHistogram struct {
	// TODO: use the hdr library
}

type Histogram struct {
	tags map[string]string
	data baseHistogram
}

type Measurement struct {
	tags map[string]string
	count uint32
	sum uint64
}

type MetricsRecord struct {
	Transaction string
	Duration time.Duration
	Status int
	Method string
	HasError bool
}

// FlushBSON is called by the reporter to generate the histograms/metrics
// message in BSON format. It send a request to the histReq channel and
// blocked in the hist channel. FlushBSON is called synchronous so it
// expects to get the result in a short time.
func (am *metricsAggregator) FlushBSON() [][]byte {
	am.rawReq <- struct{}{}
	// Don't let me get blocked here too long
	raw := <- am.raw
	return am.createMetricsMsg(raw)
}

// createMetricsMsg read the histogram and measurement data from MetricsRaw and build
// the BSON message.
func (am *metricsAggregator) createMetricsMsg(raw MetricsRaw) [][]byte {
	var bbuf bsonBuffer
	bsonBufferInit(&bbuf)

	// TODO: some of the appends are optional (only when the info is available), check and decide whether
	// TODO: to append it inside the appendXXX function. See generateMetricMessage in liboboe.
	am.appendHostname(&bbuf)
	am.appendUUID(&bbuf)
	am.appendDistro(&bbuf)
	am.appendPID(&bbuf)
	am.appendTID(&bbuf)
	am.appendSysName(&bbuf)
	am.appendVersion(&bbuf)
	am.appendIPAddresses(&bbuf)
	am.appendMAC(&bbuf)
	am.appendEC2InstanceID(&bbuf)
	am.appendEC2InstanceZone(&bbuf)
	am.appendContainerID(&bbuf)
	am.appendTimestamp(&bbuf)
	am.appendFlushInterval(&bbuf)
	am.appendTransactionNameOverflow(&bbuf)


	// TODO: continue to add measurements and other elements.

	var bufs = make([][]byte, 1)
	bufs[0] = bbuf.buf
	return bufs
}

// ProcessMetrics consumes the records sent by traces and update the histograms.
// It also generate and push the metrics event to the hist channel which is consumed by
// FlushBSON to generate the final message in BSON format.
func (am *metricsAggregator) ProcessMetrics() {
	for {
		select {
		case record := <-am.records:
			am.updateMetricsRaw(record)
		case <- am.rawReq:
			am.pushMetricsRaw()
		case <- am.exit:
			OboeLog(INFO, "Closing ProcessMetrics goroutine.", nil)
			close(am.raw)
			break
		}
	}
}

// isWithinLimit stores the transaction name into a internal set and returns true, before that
// it checks if the number of transaction names stored inside metricsAggregator is still within
// the limit. If not it returns false and does not store the transaction name.
func (am *metricsAggregator) isWithinLimit(transaction string, max int) bool {
	if _, ok := am.transNames[transaction]; !ok {
		if len(am.transNames) < max {
			am.transNames[transaction] = true
			return true
		} else {
			return false
		}
	}
	return true
}

// metricsAggregator updates the Metrics (histograms and measurements) raw data structs based on
// the MetricsRecord.
func (am *metricsAggregator) updateMetricsRaw(record MetricsRecord) {
	am.recordHistogram("", record.Duration)
	if record.Transaction {
		if am.isWithinLimit(record.Transaction, MaxTransactionNames) {
			am.recordHistogram(record.Transaction, record.Duration)
			am.processMeasurements(record.Transaction, record)
		} else {
			am.processMeasurements("other", record)
		}
	} else {
		am.processMeasurements("unknown", record)
	}
}

// recordHistogram updates the histogram based on the new MetricsRecord (transaction name and
// the duration).
func (am *metricsAggregator) recordHistogram(transaction string, duration time.Duration) {
	var tags = map[string]string{}

	if transaction {
		tags["TransactionName"] = transaction
	}

	if _, ok := am.metrics.histograms[transaction]; !ok {
		am.metrics.histograms[transaction] = newHistogram(&tags, DefaultHistogramPrecision)
	}
	am.metrics.histograms[transaction].recordValue(uint64(duration.Seconds()*1e6))
}

// processMeasurements updates the measurements struct based on the new MetricsRecord
func (am *metricsAggregator) processMeasurements(transaction string, record MetricsRecord) {
	// primary ID: TransactionName
	var primaryTags = map[string]string{}
	primaryTags["TransactionName"] = transaction
	am.recordMeasurement(&primaryTags, record.Duration)

	// secondary keys: HttpMethod
	var withMethodTags = map[string]string{}
	for k, v := range primaryTags {
		withMethodTags[k] = v
	}
	withMethodTags["HttpMethod"] = record.Method
	am.recordMeasurement(&withMethodTags, record.Duration)

	// secondary keys: HttpStatus
	var withStatusTags = map[string]string{}
	for k, v := range primaryTags {
		withStatusTags[k] = v
	}
	withStatusTags["HttpStatus"] = strconv.Itoa(record.Status)
	am.recordMeasurement(&withStatusTags, record.Duration)

	// secondary keys: Errors
	if record.HasError {
		var withErrorTags = map[string]string{}
		for k, v := range primaryTags {
			withErrorTags[k] = v
		}
		withErrorTags["Errors"] = "true"
		am.recordMeasurement(&withErrorTags, record.Duration)
	}

}

// recordMeasurement updates a particular measurement based on the tags and duration
func (am *metricsAggregator) recordMeasurement(tags *map[string]string, duration time.Duration) {
	var id string
	for k, v := range *tags {
		id += k + ":" + v + "&"
	}
	if _, ok := am.metrics.measurements[id]; !ok {
		am.metrics.measurements[id] = newMeasurement(tags)
	}
	am.metrics.measurements[id].count++
	am.metrics.measurements[id].sum += uint64(duration.Seconds()*1e6)
}

// pushMetricsRaw is called when FlushBSON requires a new histograms message
// for encoding. It pushes the newest values of the histograms to the raw channel
// which will be consumed by FlushBSON.
func (am *metricsAggregator) pushMetricsRaw() {
	am.raw <- am.metrics
}

// PushMetricsRecord is called by the Trace to record the metadata of a call, e.g., call duration,
// transaction name, status code.
func (am *metricsAggregator) PushMetricsRecord(record MetricsRecord) bool {
	select {
	case am.records <- record:
		return true
	default:
		return false
	}
}

// recordValue records the duration to the histogram
func (hist *Histogram) recordValue(duration uint64) {
	// TODO: use the API from hdr library
}

// newHistogram creates a Histogram object with tags and precision
func newHistogram(inTags *map[string]string, precision int) *Histogram {
	var histogram = Histogram{
		tags: make(map[string]string),
	}
	for k, v := range *inTags {
		histogram.tags[k] = v
	}
	// TODO: initialize hdr histogram (Histogram.data)
	return &histogram
}

// newMeasurement creates a Measurement object with tags
func newMeasurement(inTags *map[string]string) *Measurement {
	var measurement = Measurement{
		tags: make(map[string]string),
	}
	for k, v := range *inTags {
		measurement.tags[k] = v
	}
	return &measurement
}

// newMetricsAggregator is the newMetricsAggregator initializer. Note: You still need to
// initialize the Hisogram.data each time you add a new key/value to it, as by default
// it's a nil map pointer.
func newMetricsAggregator() MetricsAggregator {
	return &metricsAggregator{
		records: make(chan MetricsRecord, MetricsRecordMaxSize),
		rawReq: make(chan struct{}),
		raw: make(chan MetricsRaw),
		exit: make(chan struct{}),
		transNames: make(map[string]bool),
		metrics: MetricsRaw{
			histograms: make(map[string]*Histogram),
			measurements: make(map[string]*Measurement),
		},
		cachedSysMeta: make(map[string]string),
	}
}


