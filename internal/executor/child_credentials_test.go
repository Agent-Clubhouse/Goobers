package executor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestTrustedLocalShellEnvironment(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-provider-secret")
	t.Setenv("ARBITRARY_SECRET", "ambient-extra-secret")
	t.Setenv("HTTP_PROXY", "https://user:password@proxy")
	executor, _ := newTestExecutor(t, nil)
	executor.ExtraEnvAllowlist = []string{"GH_TOKEN", "ARBITRARY_SECRET"}
	env := baseEnvelope(t)
	home := t.TempDir()
	ctx, err := credentials.WithChildCeiling(t.Context(), credentials.NewChildCeiling(false, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = credentials.WithTrustedLocalProcess(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "env")
	result, err := executor.Run(ctx, env, apiv1.DeterministicRun{
		Command: []string{binary, "-test.run=^TestTrustedLocalShellEnvironmentHelper$", "--", "--trusted-env", output},
		Env:     map[string]string{"HOME": "/overridden", "GIT_CONFIG_GLOBAL": "/ambient-git", "RESULT": "declared-value"},
	})
	if err != nil || result.Status != apiv1.ResultSuccess {
		t.Fatalf("trusted local command: %+v %v", result, err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"ambient-provider", "ambient-extra", "user:password", "/overridden", "/ambient-git"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("authentication environment escaped: %s", forbidden)
		}
	}
	if !strings.Contains(string(data), "HOME="+home+"\n") || !strings.Contains(string(data), "RESULT=declared-value\n") {
		t.Fatal("private home or declared environment missing")
	}
}

func TestTrustedLocalShellEnvironmentHelper(t *testing.T) {
	args := os.Args
	if len(args) < 3 || args[len(args)-2] != "--trusted-env" {
		return
	}
	if err := os.WriteFile(args[len(args)-1], []byte(strings.Join(os.Environ(), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
