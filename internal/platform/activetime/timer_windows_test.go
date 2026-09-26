//go:build windows

package activetime

import (
	"testing"
	"time"
)

func TestUnbiasedUptimeAdvances(t *testing.T) {
	before, err := unbiasedUptime()
	if err != nil {
		t.Fatalf("unbiasedUptime: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	after, err := unbiasedUptime()
	if err != nil {
		t.Fatalf("unbiasedUptime: %v", err)
	}
	if after <= before {
		t.Fatalf("unbiased uptime did not advance: before=%s after=%s", before, after)
	}
}
