package executor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
)

func TestChildLocalShellRefusesAmbientAuthenticationBeforeEffects(t *testing.T) {
	t.Setenv("GH_TOKEN", "inherited-provider-secret")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", "ambient-helper")
	executor, recorder := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	ctx, err := credentials.WithChildCeiling(t.Context(), credentials.NewChildCeiling(false, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Run(ctx, env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "touch escaped"}})
	if !errors.Is(err, credentials.ErrChildAuthenticationIsolation) {
		t.Fatalf("local child: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Workspace, "escaped")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("child executed before isolation refusal")
	}
	if len(recorder.recorded) != 0 {
		t.Fatal("refused child published process output")
	}
}
