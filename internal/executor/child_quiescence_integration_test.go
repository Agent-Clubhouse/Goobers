//go:build integration && (linux || darwin)

package executor

import (
	"errors"
	"github.com/goobers/goobers/test/testsupport/testdep"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/platform/proc"
)

func TestIntegrationChildShellNormalReturnJoinsBackgroundWorkspaceWriter(t *testing.T) {
	testdep.Require(t, "sh")
	executor, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	ctx, proof := invoke.WithWorkspaceQuiescence(t.Context())
	result, err := executor.Run(ctx, env, apiv1.DeterministicRun{Command: []string{"/bin/sh", "-c", "(while :; do printf x >> writes; sleep 0.02; done) >/dev/null 2>&1 & sleep 0.1; exit 0"}})
	if err != nil || result.Status != apiv1.ResultSuccess {
		t.Fatalf("shell=%+v %v", result, err)
	}
	if err := proof.Verify(); err != nil {
		if !errors.Is(err, proc.ErrQuiescenceUnobservable) {
			t.Fatal(err)
		}
		t.Log("host cannot inventory owned writers; terminal custody correctly remains refused")
	}
	before, err := os.ReadFile(filepath.Join(env.Workspace, "writes"))
	if err != nil || len(before) == 0 {
		t.Fatalf("writer did not run: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.ReadFile(filepath.Join(env.Workspace, "writes"))
	if err != nil || len(after) != len(before) {
		t.Fatal("background writer outlived normal child invocation return")
	}
}
