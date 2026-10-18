package main

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
)

func TestProviderCommandContextUsesStageBudget(t *testing.T) {
	const stageBudget = 200 * time.Millisecond
	t.Setenv(executor.InputEnvVar(executor.InputTimeout), stageBudget.String())

	start := time.Now()
	ctx, cancel := providerCommandContext()
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("provider command context has no deadline")
	}
	got := deadline.Sub(start)
	want := providerCommandBudget(stageBudget)
	if got < want-10*time.Millisecond || got > want+10*time.Millisecond {
		t.Fatalf("provider command deadline = %s, want %s", got, want)
	}
	if got >= stageBudget {
		t.Fatalf("provider command deadline = %s, want less than stage budget %s", got, stageBudget)
	}
}

// TestStageTimeoutPrefersExecutorEffectiveDeadline is #5572: a stage whose
// deadline comes from timeoutSeconds or the runner default has no
// inputs.timeout, so stageTimeout must read the executor-injected effective
// deadline instead of assuming the built-in 10m default.
func TestStageTimeoutPrefersExecutorEffectiveDeadline(t *testing.T) {
	t.Setenv(executor.InputEnvVar(executor.InputTimeout), "")
	t.Setenv(executor.StageTimeoutEnvVar, "")
	if got := stageTimeout(); got != executor.DefaultTimeout {
		t.Fatalf("stageTimeout() with nothing injected = %s, want %s", got, executor.DefaultTimeout)
	}

	t.Setenv(executor.StageTimeoutEnvVar, "5m0s")
	if got := stageTimeout(); got != 5*time.Minute {
		t.Fatalf("stageTimeout() = %s, want the injected 5m0s", got)
	}

	t.Setenv(executor.InputEnvVar(executor.InputTimeout), "20m")
	if got := stageTimeout(); got != 5*time.Minute {
		t.Fatalf("stageTimeout() = %s, want the enforced 5m0s over a superseded inputs.timeout", got)
	}

	t.Setenv(executor.StageTimeoutEnvVar, "garbage")
	if got := stageTimeout(); got != 20*time.Minute {
		t.Fatalf("stageTimeout() with an unparseable injection = %s, want inputs.timeout 20m", got)
	}
}
