//go:build windows

package main

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func TestListenDashboardReportsConflictAndCanIncrementPastExcludedWindowsPort(t *testing.T) {
	err := fmt.Errorf("listen tcp 127.0.0.1:51225: %w", windows.WSAEACCES)
	if !dashboardAddressInUse(err) {
		t.Fatalf("dashboardAddressInUse(%v) = false, want true", err)
	}
}
