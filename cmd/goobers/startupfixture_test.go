package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// runUpThroughStartup exercises successful startup before asking the daemon to
// stop. A pre-canceled context or elapsed wall-clock guess can stop at an earlier
// phase, so neither proves the startup behavior these fixtures assert.
func runUpThroughStartup(t *testing.T, args []string, stdout, stderr io.Writer) int {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	output, diagnostics := newDaemonOutput(), newDaemonOutput()
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, args, output, diagnostics) }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("daemon did not stop during test cleanup")
			}
		}
	}()
	select {
	case <-output.started:
		if !strings.Contains(output.String(), "startup phase=ready status=done") {
			t.Fatalf("daemon announced startup without readiness: stdout=%q stderr=%q", output.String(), diagnostics.String())
		}
	case code := <-done:
		joined = true
		t.Fatalf("daemon exited before startup: code=%d stdout=%q stderr=%q", code, output.String(), diagnostics.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("daemon did not complete startup: stdout=%q stderr=%q", output.String(), diagnostics.String())
	}
	cancel()
	select {
	case code := <-done:
		joined = true
		if _, err := io.WriteString(stdout, output.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(stderr, diagnostics.String()); err != nil {
			t.Fatal(err)
		}
		return code
	case <-time.After(10 * time.Second):
		t.Fatalf("daemon did not stop after readiness: stdout=%q stderr=%q", output.String(), diagnostics.String())
		return 1
	}
}
