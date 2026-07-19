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
