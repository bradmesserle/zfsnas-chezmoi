package system

import "log"

// DebugMode enables verbose logging when set to true (via --debug flag).
var DebugMode bool

func debugLog(format string, args ...interface{}) {
	if DebugMode {
		log.Printf("[debug] "+format, args...)
	}
}
