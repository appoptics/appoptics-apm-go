// Copyright (C) 2016 Librato, Inc. All rights reserved.

package traceview

import (
	"errors"
	"testing"
	"time"

	g "github.com/librato/go-traceview/v1/tv/internal/graphtest"
	"github.com/stretchr/testify/assert"
)

// Exercise sampling rate logic:
func TestSampleRequest(t *testing.T) {
	_ = SetTestReporter() // set up test reporter
	sampled := 0
	total := 1000
	for i := 0; i < total; i++ {
		if ok, _, _ := shouldTraceRequest(testLayer, ""); ok {
			sampled++
		}
	}
	t.Logf("Sampled %d / %d requests", sampled, total)

	if sampled == 0 {
		t.Errorf("Expected to sample a request.")
	}
}

func TestNullReporter(t *testing.T) {
	globalReporter = &nullReporter{}
	assert.False(t, globalReporter.IsOpen())

	// The nullReporter should seem like a regular reporter and not break
	assert.NotPanics(t, func() {
		ctx := newContext()
		err := ctx.ReportEvent("info", testLayer, "Controller", "test_controller", "Action", "test_action")
		assert.NoError(t, err)
	})

	buf := []byte("xxx")
	cnt, err := globalReporter.WritePacket(buf)
	assert.NoError(t, err)
	assert.Equal(t, len(buf), cnt)
}

func TestNewReporter(t *testing.T) {
	assert.IsType(t, &udpReporter{}, newUDPReporter())
	t.Logf("Forcing UDP listen error for invalid port 7777831")
	udpReporterAddr = "127.0.0.1:777831"
	assert.IsType(t, &nullReporter{}, newUDPReporter())
	udpReporterAddr = "127.0.0.1:7831"
}

// dependency injection for os.Hostname and net.{ResolveUDPAddr/DialUDP}
type failHostnamer struct{}

func (h failHostnamer) Hostname() (string, error) {
	return "", errors.New("couldn't resolve hostname")
}
func TestCacheHostname(t *testing.T) {
	assert.IsType(t, &udpReporter{}, newUDPReporter())
	t.Logf("Forcing hostname error: 'Unable to get hostname' log message expected")
	cacheHostname(failHostnamer{})
	assert.IsType(t, &nullReporter{}, newUDPReporter())
}

func TestReportEvent(t *testing.T) {
	r := SetTestReporter()
	ctx := newTestContext(t)
	assert.Error(t, reportEvent(r, ctx, nil))
	assert.Len(t, r.Bufs, 0) // no reporting

	// mismatched task IDs
	ev, err := ctx.newEvent(LabelExit, testLayer)
	assert.NoError(t, err)
	assert.Error(t, reportEvent(r, nil, ev))
	assert.Len(t, r.Bufs, 0) // no reporting

	ctx2 := newTestContext(t)
	e2, err := ctx2.newEvent(LabelEntry, "layer2")
	assert.NoError(t, err)
	assert.Error(t, reportEvent(r, ctx2, ev))
	assert.Error(t, reportEvent(r, ctx, e2))

	// successful event
	assert.NoError(t, reportEvent(r, ctx, ev))
	r.Close(1)
	assert.Len(t, r.Bufs, 1)

	// re-report: shouldn't work (op IDs the same, reporter closed)
	assert.Error(t, reportEvent(r, ctx, ev))

	g.AssertGraph(t, r.Bufs, 1, g.AssertNodeMap{
		{"go_test", "exit"}: {},
	})
}

// test behavior of the TestReporter
func TestTestReporter(t *testing.T) {
	r := SetTestReporter()
	r.Close(1) // wait on event that will never be reported: causes timeout
	assert.Len(t, r.Bufs, 0)

	r = SetTestReporter()
	go func() { // simulate late event
		time.Sleep(100 * time.Millisecond)
		ctx := newTestContext(t)
		ev, err := ctx.newEvent(LabelExit, testLayer)
		assert.NoError(t, err)
		assert.NoError(t, reportEvent(r, ctx, ev))
	}()
	r.Close(1) // wait on late event -- blocks until timeout or event received
	assert.Len(t, r.Bufs, 1)

	// send an event after calling Close -- should panic
	assert.Panics(t, func() {
		ctx := newTestContext(t)
		ev, err := ctx.newEvent(LabelExit, testLayer)
		assert.NoError(t, err)
		assert.NoError(t, reportEvent(r, ctx, ev))
	})
}
