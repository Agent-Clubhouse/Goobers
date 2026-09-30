package main

import (
	"net/http"
	"time"
)

var networkModeNames = [...]string{"healthy", "503", "timeout", "429", "reset", "ambiguous-ack"}

// Five equal fault phases followed by equal-duration healthy recovery. Keep
// these isolated from the historical outage-recovery scenario's timings.
func networkFaultMode(elapsed, duration time.Duration) int32 {
	phase := int(elapsed / max(duration/6, time.Nanosecond))
	if phase >= 5 {
		return 0
	}
	return int32(phase + 1)
}

func respondNetworkFault(w http.ResponseWriter, mode int32) {
	switch mode {
	case 3:
		w.WriteHeader(http.StatusTooManyRequests)
	case 4, 5:
		// The collector records envelopes only in mode 5: close the TCP
		// connection without an acknowledgement after accepting those IDs.
		conn, _, err := w.(http.Hijacker).Hijack()
		must(err)
		must(conn.Close())
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}
