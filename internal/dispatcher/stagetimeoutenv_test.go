package dispatcher

import (
	"testing"
	"time"
)

// TestCLIStagePodCarriesTheEnforcedStageTimeout is #5572's pod half: the
// deadline dispatch-exec enforces (GOOBERS_STAGE_TIMEOUT) is privileged and
// stripped from the stage, so a goobers-CLI stage gets the same value under
// the executor's effective-timeout name — otherwise a pod-dispatched provider
// command budgets its context, and its rate-limit sleeps, against the 10m
// default while the pod kills it at the declared 5m.
func TestCLIStagePodCarriesTheEnforcedStageTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{"declared limit", 5 * time.Minute, "5m0s"},
		{"dispatcher default", 0, DefaultStageTimeout.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt := cliAttempt()
			attempt.Timeout = tc.timeout
			pod, err := RenderPod(testConfig(), attempt, linuxRunner())
			if err != nil {
				t.Fatalf("RenderPod: %v", err)
			}
			env := podEnvMap(pod)
			if got := env[executorStageTimeoutEnv]; got != tc.want {
				t.Fatalf("%s = %q, want %q", executorStageTimeoutEnv, got, tc.want)
			}
			if got := env[EnvStageTimeout]; got != tc.want {
				t.Fatalf("%s = %q, want the same enforced %q", EnvStageTimeout, got, tc.want)
			}
		})
	}

	attempt := cliAttempt()
	attempt.CLIStage = false
	pod, err := RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if value, stamped := podEnvMap(pod)[executorStageTimeoutEnv]; stamped {
		t.Fatalf("%s = %q on a non-CLI stage; only goobers-CLI stages receive run context", executorStageTimeoutEnv, value)
	}
}
