package main

import (
	"slices"
	"strings"
	"testing"
)

// Race build cache for the unit shards.
//
// setup-go's cache is written once per go.sum by the first Linux job to finish,
// a non-race build, so every race shard used to spend its first minutes
// compiling race-instrumented test binaries cold. race-build-cache-warm
// compiles exactly what the shards compile on each push to main and saves it;
// the shards restore it and never save. These tests hold the parts that rot
// silently: the key derivation both sides compute, the build flags the warm
// build uses (a cache written with different flags is never hit), and who may
// write or delete an entry.

const (
	raceCacheWarmJob    = "race-build-cache-warm"
	raceCachePruneJob   = "race-build-cache-prune"
	raceCacheKeyStep    = "Resolve race build cache key"
	raceCacheRestore    = "Restore race build cache"
	raceCacheKeyStepID  = "race-build-key"
	raceCacheKeyExpr    = "${{ steps." + raceCacheKeyStepID + ".outputs.key }}"
	raceCachePathExpr   = "${{ steps." + raceCacheKeyStepID + ".outputs.path }}"
	raceCacheSaveAction = "actions/cache/save@"
)

func TestRaceBuildCacheKeyDerivationIsShared(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)
	shard, warm := workflow.Jobs["unit"], workflow.Jobs[raceCacheWarmJob]
	if len(shard.Steps) == 0 || len(warm.Steps) == 0 {
		t.Fatalf("CI workflow must define both the unit and %s jobs", raceCacheWarmJob)
	}

	for _, name := range []string{raceCacheKeyStep, raceCacheRestore} {
		shardStep, warmStep := shard.step(t, name), warm.step(t, name)
		if !slices.Equal(significantLines(shardStep.Run), significantLines(warmStep.Run)) ||
			shardStep.Uses != warmStep.Uses || !sameWith(shardStep, warmStep) || shardStep.ID != warmStep.ID {
			t.Errorf("step %q has drifted between unit and %s; the warm job would publish an entry the shards never ask for", name, raceCacheWarmJob)
		}
	}

	derivation := strings.Join(significantLines(shard.step(t, raceCacheKeyStep).Run), "\n")
	for _, input := range []string{"GOVERSION", "ImageOS", "go.sum", "GITHUB_SHA", "GOCACHE"} {
		if !strings.Contains(derivation, input) {
			t.Errorf("race build cache key derivation must use %s", input)
		}
	}
	restore := shard.step(t, raceCacheRestore)
	if restore.with("path") != raceCachePathExpr || restore.with("key") != raceCacheKeyExpr {
		t.Errorf("unit restore path/key = %q/%q, want %q/%q", restore.with("path"), restore.with("key"), raceCachePathExpr, raceCacheKeyExpr)
	}
	if restore.with("restore-keys") == "" {
		t.Error("unit must restore the newest earlier entry by prefix: the exact key names a commit that has not been warmed yet")
	}
	if shard.stepIndex(raceCacheRestore) > shard.stepIndex("Unit suite (-race, shard ${{ matrix.shard }})") {
		t.Error("unit must restore the race build cache before the suite runs")
	}
}

func sameWith(a, b ciStep) bool {
	if len(a.With) != len(b.With) {
		return false
	}
	for key := range a.With {
		if a.with(key) != b.with(key) {
			return false
		}
	}
	return true
}

func TestRaceBuildCacheWarmBuildsWithTheShardsFlags(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)
	warm := workflow.Jobs[raceCacheWarmJob]
	build := warm.step(t, "Compile every race unit test binary (no tests run)")
	shardRun := workflow.Jobs["unit"].step(t, "Unit suite (-race, shard ${{ matrix.shard }})").Run
	if strings.TrimSpace(build.Run) != strings.TrimSpace(shardRun) {
		t.Errorf("warm build runs %q, want the shards' own command %q", build.Run, shardRun)
	}
	want := map[string]string{"GOOBERS_CI_COMPILE_ONLY": "1", "GOOBERS_CI_COVERAGE": "0"}
	for name, value := range want {
		if build.Env[name] != value {
			t.Errorf("warm build env %s = %q, want %q", name, build.Env[name], value)
		}
	}
	for name := range build.Env {
		if _, ok := want[name]; !ok {
			t.Errorf("warm build sets %s; any toggle beyond compile-only and the shards' coverage-off posture risks changing build flags", name)
		}
	}
}

func TestRaceBuildCacheIsWrittenOnlyByTheMainWarmJob(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)
	warm := workflow.Jobs[raceCacheWarmJob]
	if warm.If != "${{ github.event_name == 'push' }}" {
		t.Errorf("%s condition = %q; it must run only on pushes to main, where the entry is visible to every pull request", raceCacheWarmJob, warm.If)
	}
	if !warm.ContinueOnError {
		t.Errorf("%s must be continue-on-error: it is an optimisation", raceCacheWarmJob)
	}
	save := warm.stepUsing(t, raceCacheSaveAction)
	if save.with("path") != raceCachePathExpr || save.with("key") != raceCacheKeyExpr {
		t.Errorf("%s save path/key = %q/%q, want %q/%q", raceCacheWarmJob, save.with("path"), save.with("key"), raceCachePathExpr, raceCacheKeyExpr)
	}
	for _, step := range workflow.Jobs["unit"].Steps {
		if strings.HasPrefix(step.Uses, raceCacheSaveAction) || strings.HasPrefix(step.Uses, "actions/cache@") {
			t.Errorf("unit step %q saves a cache; five shards uploading ~1 GB each would evict every other entry", step.Name)
		}
	}
	for _, gate := range []string{"required-ci", "escalate-main-failure"} {
		for _, job := range []string{raceCacheWarmJob, raceCachePruneJob} {
			if slices.Contains(workflow.Jobs[gate].Needs, job) {
				t.Errorf("%s must not depend on %s; cache warming is not a gate", gate, job)
			}
		}
	}
}

func TestRaceBuildCachePruneRunsNoRepositoryCode(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)
	if got := workflow.Jobs[raceCacheWarmJob].Permissions["actions"]; got == "write" {
		t.Errorf("%s compiles repository code and must not hold actions: write", raceCacheWarmJob)
	}
	prune := workflow.Jobs[raceCachePruneJob]
	if prune.Permissions["actions"] != "write" {
		t.Errorf("%s needs actions: write to delete superseded entries", raceCachePruneJob)
	}
	if !slices.Equal(prune.Needs, []string{raceCacheWarmJob}) || !strings.Contains(prune.If, "saved-key") {
		t.Errorf("%s must run after, and only when, %s saved a new entry (needs %q, if %q)", raceCachePruneJob, raceCacheWarmJob, prune.Needs, prune.If)
	}
	for _, step := range prune.Steps {
		if step.Uses != "" {
			t.Errorf("%s step %q uses %q; the job holding a cache-deleting token checks out and runs nothing", raceCachePruneJob, step.Name, step.Uses)
		}
		if strings.Contains(step.Run, "gh cache delete") && !strings.Contains(step.Run, "--key go-race-build-v1-") {
			t.Errorf("%s must only ever delete race build cache entries", raceCachePruneJob)
		}
	}
}
