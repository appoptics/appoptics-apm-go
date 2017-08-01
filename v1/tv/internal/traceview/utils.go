// Copyright (C) 2017 Librato, Inc. All rights reserved.

package traceview

import (
	"log"
)

type DebugLevel uint8

const (
	DEBUG DebugLevel = iota
	INFO
	WARNING
	ERROR
)

//OboeLog print logs based on the debug level.
func OboeLog(level DebugLevel, msg string, err error) {
	if !debugLog { // remove it
		return
	}
	if level >= debugLevel {
		log.Printf("%s: %v", msg, err)
	}
}
