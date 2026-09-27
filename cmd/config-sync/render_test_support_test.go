package main

import (
	"runtime"
	"testing"
)

func requireRenderSupport(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("atomic manifest publication is unsupported on " + runtime.GOOS)
	}
}
