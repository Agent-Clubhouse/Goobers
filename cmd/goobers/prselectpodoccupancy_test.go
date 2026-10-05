package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/providers"
)

// Production regression: goobers/merge-review pr-select on a pod failed 34/34
// with "read instance.yaml: open instance.yaml: no such file or directory"
// because the pod guard keyed on GOOBERS_POD_TOKEN, which dispatch-exec strips
// from every stage's environment.
func TestPRSelectBranchOccupanciesPodStageEnvironment(t *testing.T) {
	t.Setenv(dispatcher.EnvPodToken, "secret-token")
	t.Setenv(dispatcher.EnvPodAttempt, "1")
	t.Setenv(dispatcher.EnvStageIsCLI, "true")
	t.Setenv(dispatcher.EnvRunID, "run-1")

	// Replace the process environment with exactly what the dispatcher hands a
	// goobers-CLI stage: the pod token is gone, run identity remains.
	present := map[string]bool{}
	for _, kv := range stageEnvironment() {
		name, _, _ := strings.Cut(kv, "=")
		present[name] = true
	}
	for _, name := range dispatcher.DispatcherPrivilegedEnv {
		if !present[name] {
			t.Setenv(name, "")
		}
	}
	if os.Getenv(dispatcher.EnvPodToken) != "" {
		t.Fatal("test premise broken: stage environment still carries the pod token")
	}

	got, err := prSelectBranchOccupancies(context.Background(), t.TempDir(), providers.RepositoryRef{})
	if err != nil || len(got) != 0 {
		t.Fatalf("pod stage occupancies = %v, %v; want empty, nil", got, err)
	}
}

func TestPRSelectBranchOccupanciesMissingInstanceConfigIsEmpty(t *testing.T) {
	t.Setenv(dispatcher.EnvPodToken, "")
	t.Setenv(dispatcher.EnvPodAttempt, "")
	got, err := prSelectBranchOccupancies(context.Background(), t.TempDir(), providers.RepositoryRef{})
	if err != nil || len(got) != 0 {
		t.Fatalf("no instance.yaml occupancies = %v, %v; want empty, nil", got, err)
	}
}
