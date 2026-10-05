//go:build integration && (linux || darwin)

package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/platform/proc"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationChildYieldJoinsWorkspaceWriterBeforeAcknowledgement(t *testing.T) {
	testdep.Require(t, "sh")
	root := t.TempDir()
	ctx, proof := invoke.WithWorkspaceQuiescence(t.Context())
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := (ExecProcessRunner{}).Run(ctx, ProcessRequest{Command: []string{"/bin/sh", "-c", "(while :; do printf x >> writes; sleep 0.02; done) & wait"}, Dir: root, Env: []string{"PATH=/usr/bin:/bin"}})
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	for {
		if info, err := os.Stat(filepath.Join(root, "writes")); err == nil && info.Size() > 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("writer did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("yield cancellation=%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writer join hung")
	}
	if err := proof.Verify(); err != nil {
		if !errors.Is(err, proc.ErrQuiescenceUnobservable) {
			t.Fatal(err)
		}
		t.Log("host cannot inventory writers; snapshot correctly refused despite process cancellation")
	}
	before, err := os.ReadFile(filepath.Join(root, "writes"))
	if err != nil {
		t.Fatal(err)
	}
	// A live descendant would append repeatedly during this observation window.
	time.Sleep(100 * time.Millisecond)
	after, err := os.ReadFile(filepath.Join(root, "writes"))
	if err != nil || len(after) != len(before) {
		t.Fatal("writer mutated workspace after acknowledgement")
	}
}
