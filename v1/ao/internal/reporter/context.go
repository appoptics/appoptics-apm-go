// Copyright (C) 2016 Librato, Inc. All rights reserved.

package reporter

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/appoptics/appoptics-apm-go/v1/ao/internal/log"
	"github.com/pkg/errors"
)

const (
	oboeMetadataStringLen = 60
	maskTaskIDLen         = 0x03
	maskOpIDLen           = 0x08
	maskHasOptions        = 0x04
	maskVersion           = 0xF0

	xtrCurrentVersion      = 2
	oboeMaxTaskIDLen       = 20
	oboeMaxOpIDLen         = 8
	oboeMaxMetadataPackLen = 512
)

// x-trace flags
const (
	XTR_FLAGS_NONE    = 0x0
	XTR_FLAGS_SAMPLED = 0x1
)

const HTTPHeaderXTraceOptions = "X-Trace-Options"
const HTTPHeaderXTraceOptionsSignature = "X-Trace-Options-Signature"
const HTTPHeaderXTraceOptionsResponse = "X-Trace-Options-Response"

var (
	errInvalidTaskID = errors.New("invalid task id")
)

// All-zero slice to validate the task ID, do not modify it
var allZeroTaskID = make([]byte, oboeMaxTaskIDLen)

// orchestras tune to the oboe.
type oboeIDs struct{ taskID, opID []byte }

func (ids oboeIDs) validate() error {
	if bytes.Compare(allZeroTaskID, ids.taskID) != 0 {
		return nil
	} else {
		return errInvalidTaskID
	}
}

type oboeMetadata struct {
	version uint8
	ids     oboeIDs
	taskLen int
	opLen   int
	flags   uint8
}

type oboeContext struct {
	metadata oboeMetadata
	txCtx    *transactionContext
}

type transactionContext struct {
	name string
	// if the trace/transaction is enabled (defined by per-URL transaction filtering)
	enabled bool
	sync.RWMutex
}

type KVMap map[string]interface{}

type Overrides struct {
	ExplicitTS    time.Time
	ExplicitMdStr string
}

// ContextOptions defines the options of creating a context.
type ContextOptions struct {
	// MdStr is the string representation of the X-Trace ID.
	MdStr string
	// URL is used to do the URL-based transaction filtering.
	URL string
	// XTraceOptions represents the X-Trace-Options header.
	XTraceOptions string
	// CB is the callback function to produce the KVs.
	// XTraceOptionsSignature represents the X-Trace-Options-Signature header.
	XTraceOptionsSignature string
	Overrides              Overrides
	CB                     func() KVMap
}

// ValidMetadata checks if a metadata string is valid.
func ValidMetadata(mdStr string) bool {
	md := &oboeMetadata{}
	md.Init()
	if err := md.FromString(mdStr); err != nil {
		return false
	}
	return true
}

func (md *oboeMetadata) Init() {
	if md == nil {
		return
	}
	md.version = xtrCurrentVersion
	md.taskLen = oboeMaxTaskIDLen
	md.opLen = oboeMaxOpIDLen
	md.ids.taskID = make([]byte, oboeMaxTaskIDLen)
	md.ids.opID = make([]byte, oboeMaxOpIDLen)
}

// randReader provides random IDs, and can be overridden for testing.
// set by default to read from the crypto/rand Reader.
var randReader = rand.Reader

func (md *oboeMetadata) SetRandom() error {
	if md == nil {
		return errors.New("md.SetRandom: nil md")
	}

	if err := md.SetRandomTaskID(randReader); err != nil {
		return err
	}
	return md.SetRandomOpID()
}

// SetRandomTaskID randomize the task ID. It will retry if the random reader returns
// an error or produced task ID is all-zero, which rarely happens though.
func (md *oboeMetadata) SetRandomTaskID(rand io.Reader) (err error) {
	retried := 0
	for retried < 2 {
		if _, err = rand.Read(md.ids.taskID); err != nil {
			break
		}

		if err = md.ids.validate(); err != nil {
			retried++
			continue
		}
		break
	}

	return err
}

func (md *oboeMetadata) SetRandomOpID() error {
	_, err := randReader.Read(md.ids.opID)
	return err
}

func (ids *oboeIDs) setOpID(opID []byte) {
	copy(ids.opID, opID)
}

/*
 * Pack a metadata struct into a buffer.
 *
 * md       - pointer to the metadata struct
 * task_len - the task_id length to take
 * op_len   - the op_id length to take
 * buf      - the buffer to pack the metadata into
 * buf_len  - the space available in the buffer
 *
 * returns the length of the packed metadata, in terms of uint8_ts.
 */
func (md *oboeMetadata) Pack(buf []byte) (int, error) {
	if md == nil {
		return 0, errors.New("md.Pack: nil md")
	}
	if md.taskLen == 0 || md.opLen == 0 {
		return 0, errors.New("md.Pack: invalid md (0 len)")
	}

	reqLen := md.taskLen + md.opLen + 2

	if len(buf) < reqLen {
		return 0, errors.New("md.Pack: buf too short to pack")
	}

	var taskBits byte

	/*
	 * Flag field layout:
	 *     7    6     5     4     3     2     1     0
	 * +-----+-----+-----+-----+-----+-----+-----+-----+
	 * |                       |     |     |           |
	 * |        version        | oid | opt |    tid    |
	 * |                       |     |     |           |
	 * +-----+-----+-----+-----+-----+-----+-----+-----+
	 *
	 * tid - task id length
	 *          0 <~> 4, 1 <~> 8, 2 <~> 12, 3 <~> 20
	 * oid - op id length
	 *          (oid + 1) * 4
	 * opt - are options present
	 *
	 * version - the version of X-Trace
	 */
	taskBits = (uint8(md.taskLen) >> 2) - 1

	buf[0] = md.version << 4
	if taskBits == 4 {
		buf[0] |= 3
	} else {
		buf[0] |= taskBits
	}
	buf[0] |= ((uint8(md.opLen) >> 2) - 1) << 3

	copy(buf[1:1+md.taskLen], md.ids.taskID)
	copy(buf[1+md.taskLen:1+md.taskLen+md.opLen], md.ids.opID)
	buf[1+md.taskLen+md.opLen] = md.flags

	return reqLen, nil
}

func (md *oboeMetadata) Unpack(data []byte) error {
	if md == nil {
		return errors.New("md.Unpack: nil md")
	}

	if len(data) == 0 { // no header to read
		return errors.New("md.Unpack: empty buf")
	}

	flag := data[0]
	var taskLen, opLen int
	var version uint8

	/* don't recognize this? */
	if (flag&maskVersion)>>4 != xtrCurrentVersion {
		return errors.New("md.Unpack: unrecognized X-Trace version")
	}
	version = (flag & maskVersion) >> 4

	taskLen = (int(flag&maskTaskIDLen) + 1) << 2
	if taskLen == 16 {
		taskLen = 20
	}
	opLen = ((int(flag&maskOpIDLen) >> 3) + 1) << 2

	/* do header lengths describe reality? */
	if (taskLen + opLen + 2) > len(data) {
		return errors.New("md.Unpack: wrong header length")
	}

	md.version = version
	md.taskLen = taskLen
	md.opLen = opLen

	md.ids.taskID = data[1 : 1+taskLen]
	md.ids.opID = data[1+taskLen : 1+taskLen+opLen]
	md.flags = data[1+taskLen+opLen]

	return nil
}

func (md *oboeMetadata) FromString(buf string) error {
	if md == nil {
		return errors.New("md.FromString: nil md")
	}

	ubuf := make([]byte, oboeMaxMetadataPackLen)

	// a hex string's length would be an even number
	if len(buf)%2 == 1 {
		return errors.New("md.FromString: hex not even")
	}

	// check if there are more hex bytes than we want
	if len(buf)/2 > oboeMaxMetadataPackLen {
		return errors.New("md.FromString: too many hex bytes")
	}

	// invalid hex?
	ret, err := hex.Decode(ubuf, []byte(buf))
	if ret != len(buf)/2 || err != nil {
		return errors.New("md.FromString: hex not valid")
	}
	ubuf = ubuf[:ret] // truncate buffer to fit decoded bytes
	err = md.Unpack(ubuf)
	if err != nil {
		return err
	}
	return md.ids.validate()
}

func (md *oboeMetadata) ToString() (string, error) {
	if md == nil {
		return "", errors.New("md.ToString: nil md")
	}
	buf := make([]byte, 64)
	result, err := md.Pack(buf)
	if err != nil {
		return "", err
	}
	// encode as hex
	enc := make([]byte, 2*result)
	l := hex.Encode(enc, buf[:result])
	return strings.ToUpper(string(enc[:l])), nil
}

func (md *oboeMetadata) opString() string {
	enc := make([]byte, 2*md.opLen)
	l := hex.Encode(enc, md.ids.opID[:md.opLen])
	return strings.ToUpper(string(enc[:l]))
}

func (md *oboeMetadata) isSampled() bool {
	return md.flags&XTR_FLAGS_SAMPLED != 0
}

// A Context is an oboe context that may or not be tracing.
type Context interface {
	ReportEvent(label Label, layer string, args ...interface{}) error
	ReportEventWithOverrides(label Label, layer string, overrides Overrides, args ...interface{}) error
	ReportEventMap(label Label, layer string, keys map[string]interface{}) error
	Copy() Context
	IsSampled() bool
	SetSampled(trace bool)
	SetEnabled(enabled bool)
	GetEnabled() bool
	SetTransactionName(name string)
	GetTransactionName() string
	MetadataString() string
	NewEvent(label Label, layer string, addCtxEdge bool) Event
	GetVersion() uint8
}

// A Event is an event that may or may not be tracing, created by a Context.
type Event interface {
	ReportContext(c Context, addCtxEdge bool, args ...interface{}) error
	MetadataString() string
}

// A nullContext is not tracing.
type nullContext struct{}
type nullEvent struct{}

func (e *nullContext) ReportEvent(label Label, layer string, args ...interface{}) error {
	return nil
}

func (e *nullContext) ReportEventWithOverrides(label Label, layer string, overrides Overrides, args ...interface{}) error {
	return nil
}

func (e *nullContext) ReportEventMap(label Label, layer string, keys map[string]interface{}) error {
	return nil
}
func (e *nullContext) Copy() Context                                         { return &nullContext{} }
func (e *nullContext) IsSampled() bool                                       { return false }
func (e *nullContext) SetSampled(trace bool)                                 {}
func (e *nullContext) SetEnabled(enabled bool)                               {}
func (e *nullContext) GetEnabled() bool                                      { return true }
func (e *nullContext) SetTransactionName(name string)                        {}
func (e *nullContext) GetTransactionName() string                            { return "" }
func (e *nullContext) MetadataString() string                                { return "" }
func (e *nullContext) NewEvent(l Label, y string, g bool) Event              { return &nullEvent{} }
func (e *nullContext) GetVersion() uint8                                     { return 0 }
func (e *nullEvent) ReportContext(c Context, g bool, a ...interface{}) error { return nil }
func (e *nullEvent) MetadataString() string                                  { return "" }

// NewNullContext returns a context that is not tracing.
func NewNullContext() Context { return &nullContext{} }

// newContext allocates a context with random metadata (for a new trace).
func newContext(sampled bool) Context {
	ctx := &oboeContext{txCtx: &transactionContext{enabled: true}}
	ctx.metadata.Init()
	if err := ctx.metadata.SetRandom(); err != nil {
		log.Infof("AppOptics rand.Read error: %v", err)
		return &nullContext{}
	}
	ctx.SetSampled(sampled)
	return ctx
}

func NewContextFromMetadataString(mdstr string) (*oboeContext, error) {
	ctx := &oboeContext{txCtx: &transactionContext{enabled: true}}
	ctx.metadata.Init()
	err := ctx.metadata.FromString(mdstr)
	return ctx, err
}

func parseTriggerTraceFlag(opts, sig string) (TriggerTraceMode, map[string]string, []string, error) {
	if opts == "" {
		return ModeTriggerTraceNotPresent, nil, nil, nil
	}

	mode := ModeInvalidTriggerTrace
	kvs := make(map[string]string)
	var ignored []string
	var ts string

	optsSlice := strings.Split(opts, ";")
	// for deduplication
	kMap := make(map[string]struct{})

	for _, opt := range optsSlice {
		kvSlice := strings.SplitN(opt, "=", 2)
		var k, v string
		k = strings.TrimSpace(kvSlice[0])

		if len(kvSlice) == 2 {
			v = strings.TrimSpace(kvSlice[1])
		} else if kvSlice[0] != "trigger-trace" {
			log.Debugf("Dangling key found: %s", kvSlice[0])
			ignored = append(ignored, k)
			continue
		}

		// ascii spaces only
		if strings.ContainsAny(k, " \t\n\v\f\r") {
			ignored = append(ignored, k)
			continue
		}

		if !(strings.HasPrefix(k, "custom-") ||
			k == "pd-keys" ||
			k == "trigger-trace" ||
			k == "ts") {
			ignored = append(ignored, k)
			continue
		}

		if k == "pd-keys" {
			k = "PDKeys"
		}

		if _, ok := kMap[k]; ok {
			log.Debugf("Duplicate key found: %s", k)
			continue
		}
		kMap[k] = struct{}{}

		if k != "ts" {
			kvs[k] = v
		} else {
			ts = v
		}
	}
	val, ok := kvs["trigger-trace"]
	if !ok {
		return ModeTriggerTraceNotPresent, kvs, ignored, nil
	}

	if val != "" {
		log.Debug("trigger-trace should not contain any value.")
		ignored = append(ignored, "trigger-trace")
		return ModeTriggerTraceNotPresent, kvs, ignored, nil
	}
	delete(kvs, "trigger-trace")

	var authErr error
	if sig != "" {
		authErr = validateXTraceOptionsSignature(sig, ts, opts)
		if authErr == nil {
			mode = ModeRelaxedTriggerTrace
		} else {
			mode = ModeInvalidTriggerTrace
		}
	} else {
		mode = ModeStrictTriggerTrace
	}

	// ignore KVs if the signature is invalid
	if mode == ModeInvalidTriggerTrace {
		kvs = nil
		ignored = nil
	}
	return mode, kvs, ignored, authErr
}

// Trigger trace signature authentication errors
const (
	ttAuthBadTimestamp   = "bad-timestamp"
	ttAuthNoSignatureKey = "no-signature-key"
	ttAuthBadSignature   = "bad-signature"
)

func validateXTraceOptionsSignature(signature, ts, data string) error {
	var err error
	ts, err = tsInScope(ts)
	if err != nil {
		return errors.New(ttAuthBadTimestamp)
	}

	token, err := getTriggerTraceToken()
	if err != nil {
		return errors.New(ttAuthNoSignatureKey)
	}

	if HmacHash(token, []byte(data)) != signature {
		return errors.New(ttAuthBadSignature)
	}
	return nil
}

func HmacHash(token, data []byte) string {
	h := hmac.New(sha1.New, token)
	h.Write(data)
	sha := hex.EncodeToString(h.Sum(nil))
	return sha
}

func getTriggerTraceToken() ([]byte, error) {
	setting, ok := getSetting("")
	if !ok {
		return nil, errors.New("failed to get settings")
	}
	if len(setting.triggerToken) == 0 {
		return nil, errors.New("no valid signature key found")
	}
	return setting.triggerToken, nil
}

func tsInScope(tsStr string) (string, error) {
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return "", errors.Wrap(err, "tsInScope")
	}

	t := time.Unix(ts, 0)
	if t.Before(time.Now().Add(time.Minute*-5)) ||
		t.After(time.Now().Add(time.Minute*5)) {
		return "", fmt.Errorf("timestamp out of scope: %s", tsStr)
	}
	return strconv.FormatInt(ts, 10), nil
}

// NewContext starts a trace for the provided URL, possibly continuing one, if
// mdStr is provided. Setting reportEntry will report an entry event before this
// function returns, calling cb if provided for additional KV pairs.
func NewContext(layer string, reportEntry bool, opts ContextOptions,
	cb func() KVMap) (ctx Context, ok bool, headers map[string]string) {

	traced := false
	addCtxEdge := false
	explicitTraceDecision := false

	tMode, tKVs, tIgnoredKeys, authErr := parseTriggerTraceFlag(opts.XTraceOptions, opts.XTraceOptionsSignature)

	var SetHeaders = func(tt string) {
		// Only set response headers when X-Trace-Options is present
		if opts.XTraceOptions == "" {
			return
		}

		if headers == nil {
			headers = make(map[string]string)
		}
		var v []string
		if tt != "" {
			v = append(v, "trigger-trace="+tt)
		}
		if tMode == ModeRelaxedTriggerTrace {
			v = append(v, "auth=ok")
		} else if authErr != nil {
			v = append(v, "auth="+authErr.Error())
		}
		headers[HTTPHeaderXTraceOptionsResponse] = strings.Join(v, ";")
	}

	defer func() {
		if len(tIgnoredKeys) != 0 {
			xTraceOptsRsp, ok := headers[HTTPHeaderXTraceOptionsResponse]
			ignored := "ignored=" + strings.Join(tIgnoredKeys, ",")
			if ok && xTraceOptsRsp != "" {
				xTraceOptsRsp = xTraceOptsRsp + ";" + ignored
			} else {
				xTraceOptsRsp = ignored
			}
			headers[HTTPHeaderXTraceOptionsResponse] = xTraceOptsRsp
		}
	}()

	continuedTrace := false

	if opts.MdStr != "" {
		var err error
		if ctx, err = NewContextFromMetadataString(opts.MdStr); err != nil {
			log.Info("passed in x-trace seems invalid, ignoring")
		} else if ctx.GetVersion() != xtrCurrentVersion {
			log.Info("passed in x-trace has wrong version, ignoring")
		} else if ctx.IsSampled() {
			ctx.MetadataString()
			log.Info("passed in x-trace is sampled")
			traced = true
			addCtxEdge = true
			continuedTrace = true
		} else {
			setting, has := getSetting(layer)
			if !has {
				SetHeaders(ttSettingsNotAvailable)
				return ctx, false, headers
			}

			_, flags, _ := mergeURLSetting(setting, opts.URL)
			ctx.SetEnabled(flags.Enabled())

			if tMode.Requested() {
				SetHeaders(ttIgnored)
			} else {
				SetHeaders(ttNotRequested)
			}

			return ctx, true, headers
		}
	} else if opts.Overrides.ExplicitMdStr != "" {
		var err error
		//take note that the ctx here will be the same as the entry event
		//we might consider randomizing the opID or the set it to zero in the future
		//for now we will just disable the opID check in reporter.prepareEvent as the Op ID will be the same
		//for context and entry event
		if ctx, err = NewContextFromMetadataString(opts.Overrides.ExplicitMdStr); err != nil {
			log.Info("passed in x-trace seems invalid, ignoring")
		} else if ctx.GetVersion() != xtrCurrentVersion {
			log.Info("passed in x-trace has wrong version, ignoring")
		} else if ctx.IsSampled() {
			log.Info("passed in x-trace is sampled")
			traced = true
			explicitTraceDecision = true
		} else {
			return ctx, true, headers
		}
	}

	if !traced {
		ctx = newContext(true)
	}

	var decision SampleDecision
	if explicitTraceDecision {
		decision = SampleDecision{trace: true}
	} else {
		decision = shouldTraceRequestWithURL(layer, traced, opts.URL, tMode)
	}
	ctx.SetEnabled(decision.enabled)

	if decision.trace {
		if reportEntry {
			var kvs map[string]interface{}
			if cb != nil {
				kvs = cb()
			}
			if len(kvs) == 0 {
				kvs = make(map[string]interface{})
			}

			for k, v := range tKVs {
				kvs[k] = v
			}

			if !continuedTrace {
				kvs["SampleRate"] = decision.rate
				kvs["SampleSource"] = decision.source
				kvs["BucketCapacity"] = fmt.Sprintf("%f", decision.bucketCap)
				kvs["BucketRate"] = fmt.Sprintf("%f", decision.bucketRate)
			}

			if tMode.Enabled() && !traced {
				kvs["TriggeredTrace"] = true
			}
			if _, ok = ctx.(*oboeContext); !ok {
				return &nullContext{}, false, headers
			}
			if err := ctx.(*oboeContext).reportEventMapWithOverrides(LabelEntry, layer, addCtxEdge, opts.Overrides, kvs); err != nil {
				return &nullContext{}, false, headers
			}
		}
		SetHeaders(decision.xTraceOptsRsp)

		return ctx, true, headers
	}

	ctx.SetSampled(false)
	SetHeaders(decision.xTraceOptsRsp)

	return ctx, true, headers

}

func (ctx *oboeContext) Copy() Context {
	md := oboeMetadata{}
	md.Init()
	copy(md.ids.taskID, ctx.metadata.ids.taskID)
	copy(md.ids.opID, ctx.metadata.ids.opID)
	md.flags = ctx.metadata.flags
	return &oboeContext{metadata: md, txCtx: ctx.txCtx}
}
func (ctx *oboeContext) IsSampled() bool { return ctx.metadata.isSampled() }

func (ctx *oboeContext) SetSampled(trace bool) {
	if trace {
		ctx.metadata.flags |= XTR_FLAGS_SAMPLED // set sampled bit
	} else {
		ctx.metadata.flags ^= XTR_FLAGS_SAMPLED // clear sampled bit
	}
}

func (ctx *oboeContext) SetEnabled(enabled bool) {
	ctx.txCtx.enabled = enabled
}

func (ctx *oboeContext) GetEnabled() bool {
	return ctx.txCtx.enabled
}

func (ctx *oboeContext) SetTransactionName(name string) {
	ctx.txCtx.Lock()
	defer ctx.txCtx.Unlock()
	ctx.txCtx.name = name
}

func (ctx *oboeContext) GetTransactionName() string {
	ctx.txCtx.RLock()
	defer ctx.txCtx.RUnlock()
	return ctx.txCtx.name
}

func (ctx *oboeContext) newEvent(label Label, layer string) (*event, error) {
	return newEvent(&ctx.metadata, label, layer, "")
}

func (ctx *oboeContext) newEventWithExplicitID(label Label, layer string, xTraceID string) (*event, error) {
	return newEvent(&ctx.metadata, label, layer, xTraceID)
}

func (ctx *oboeContext) NewEvent(label Label, layer string, addCtxEdge bool) Event {
	e, err := newEvent(&ctx.metadata, label, layer, "")
	if err != nil {
		return &nullEvent{}
	}
	if addCtxEdge {
		e.AddEdge(ctx)
	}
	return e
}

func (ctx *oboeContext) GetVersion() uint8 {
	return ctx.metadata.version
}

// Create and report and event using a map of KVs
func (ctx *oboeContext) ReportEventMap(label Label, layer string, keys map[string]interface{}) error {
	return ctx.reportEventMap(label, layer, true, keys)
}

func (ctx *oboeContext) reportEventMapWithOverrides(label Label, layer string, addCtxEdge bool, overrides Overrides, keys map[string]interface{}) error {
	var args []interface{}
	for k, v := range keys {
		args = append(args, k)
		args = append(args, v)
	}
	return ctx.reportEventWithOverrides(label, layer, addCtxEdge, overrides, args...)
}

func (ctx *oboeContext) reportEventMap(label Label, layer string, addCtxEdge bool, keys map[string]interface{}) error {
	return ctx.reportEventMapWithOverrides(label, layer, addCtxEdge, Overrides{}, keys)
}

// Create and report an event using KVs from variadic args
func (ctx *oboeContext) ReportEvent(label Label, layer string, args ...interface{}) error {
	return ctx.reportEventWithOverrides(label, layer, true, Overrides{}, args...)
}

func (ctx *oboeContext) ReportEventWithOverrides(label Label, layer string, overrides Overrides, args ...interface{}) error {
	return ctx.reportEventWithOverrides(label, layer, true, overrides, args...)
}

func (ctx *oboeContext) reportEvent(label Label, layer string, addCtxEdge bool, args ...interface{}) error {
	return ctx.reportEventWithOverrides(label, layer, addCtxEdge, Overrides{}, args)
}

// Create and report an event using KVs from variadic args
func (ctx *oboeContext) reportEventWithOverrides(label Label, layer string, addCtxEdge bool, overrides Overrides, args ...interface{}) error {
	// create new event from context
	e, err := ctx.newEventWithExplicitID(label, layer, overrides.ExplicitMdStr)
	if err != nil { // error creating event (e.g. couldn't init random IDs)
		return err
	}

	return ctx.report(e, addCtxEdge, overrides, args...)
}

// report an event using KVs from variadic args
func (ctx *oboeContext) report(e *event, addCtxEdge bool, overrides Overrides, args ...interface{}) error {
	for i := 0; i+1 < len(args); i += 2 {
		if err := e.AddKV(args[i], args[i+1]); err != nil {
			return err
		}
	}

	if addCtxEdge {
		e.AddEdge(ctx)
	}
	e.overrides = overrides
	// report event
	return e.Report(ctx)
}

func (ctx *oboeContext) MetadataString() string { return ctx.metadata.String() }

// String returns a hex string representation
func (md *oboeMetadata) String() string {
	mdStr, _ := md.ToString()
	return mdStr
}
