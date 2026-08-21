package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Declared-dependency provisioning cache (#3322).
//
// The integration tier provisions a fixed, declared dependency set (bash, bwrap,
// dirname, head, java, mkdir, mvn, sh, sleep, yes) plus the Maven repository the
// Java fixture gaggle resolves. Provisioning it from the apt mirrors on every run
// timed out at 12 minutes at least three times in one day, and each timeout cost
// a full rerun cycle on unrelated pull requests during the v0.2.0 release push.
//
// The set is declared and fixed, so the network is only justified when the set
// itself changes. These tests hold that property in place: the fast path must be
// cached artifacts with no network at all, the cache key must be a function of
// the declared set, and — the part that silently rots — a writer on the default
// branch must publish under the exact same key, because GitHub scopes a cache
// entry written by a pull-request run to that pull request alone.

const (
	declaredAptArchiveDir = "${{ env.DECLARED_APT_ARCHIVE_DIR }}"
	declaredAptKeyStepID  = "declared-apt-key"
	declaredAptKeyExpr    = "${{ steps." + declaredAptKeyStepID + ".outputs.key }}"
	declaredAptCacheStep  = "declared-apt-cache"
	declaredCacheWarmJob  = "dependency-cache-warm"
)

type ciWorkflow struct {
	Env  map[string]string `yaml:"env"`
	Jobs map[string]ciJob  `yaml:"jobs"`
}

type ciJob struct {
	Name            string   `yaml:"name"`
	If              string   `yaml:"if"`
	ContinueOnError bool     `yaml:"continue-on-error"`
	Needs           []string `yaml:"needs"`
	Steps           []ciStep `yaml:"steps"`
}

type ciStep struct {
	Name string         `yaml:"name"`
	ID   string         `yaml:"id"`
	If   string         `yaml:"if"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
}

func (s ciStep) with(key string) string {
	if s.With == nil {
		return ""
	}
	value, ok := s.With[key]
	if !ok {
		return ""
	}
	return fmt.Sprint(value)
}

func (j ciJob) step(t *testing.T, name string) ciStep {
	t.Helper()
	for _, step := range j.Steps {
		if step.Name == name {
			return step
		}
	}
	t.Fatalf("job has no step named %q; steps: %v", name, j.stepNames())
	return ciStep{}
}

func (j ciJob) stepNames() []string {
	names := make([]string, 0, len(j.Steps))
	for _, step := range j.Steps {
		names = append(names, step.Name)
	}
	return names
}

// stepUsing returns the single step whose `uses` starts with prefix.
func (j ciJob) stepUsing(t *testing.T, prefix string) ciStep {
	t.Helper()
	var found []ciStep
	for _, step := range j.Steps {
		if strings.HasPrefix(step.Uses, prefix) {
			found = append(found, step)
		}
	}
	if len(found) != 1 {
		t.Fatalf("job has %d steps using %q, want exactly one", len(found), prefix)
	}
	return found[0]
}

func (j ciJob) stepIndex(name string) int {
	for i, step := range j.Steps {
		if step.Name == name {
			return i
		}
	}
	return -1
}

func loadCIWorkflow(t *testing.T) ciWorkflow {
	t.Helper()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	var workflow ciWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse CI workflow: %v", err)
	}
	return workflow
}

// significantLines drops blank lines and comments so two shell bodies can be
// compared on what they actually do, not on how each one is annotated.
func significantLines(script string) []string {
	var lines []string
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lines = append(lines, trimmed)
	}
	return lines
}

// TestDeclaredDependencySetIsDefinedOnce pins the declared set to a single
// workflow-level definition. Two copies drift, and a drifted copy means the
// consumer and the warm job compute different cache keys — a cache that is
// written on main, read by nobody, and silently useless.
func TestDeclaredDependencySetIsDefinedOnce(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)

	packages := workflow.Env["DECLARED_APT_PACKAGES"]
	if packages == "" {
		t.Fatal("CI workflow must define DECLARED_APT_PACKAGES at workflow level so every job keys its apt cache on one definition of the declared set")
	}
	// Keep aligned with the inventory printed by `make test-integration`.
	for _, want := range []string{"bash", "bubblewrap", "coreutils", "dash", "maven"} {
		if !slices.Contains(strings.Fields(packages), want) {
			t.Errorf("DECLARED_APT_PACKAGES = %q, missing declared dependency %q", packages, want)
		}
	}
	if workflow.Env["DECLARED_APT_ARCHIVE_DIR"] == "" {
		t.Error("CI workflow must define DECLARED_APT_ARCHIVE_DIR at workflow level: the cached path has to be identical in the consumer and the warm job")
	}

	for _, name := range []string{"integration", declaredCacheWarmJob} {
		job, ok := workflow.Jobs[name]
		if !ok {
			t.Fatalf("CI workflow has no %q job", name)
		}
		for _, step := range job.Steps {
			if step.Run == "" {
				continue
			}
			if strings.Contains(step.Run, "install "+packages) {
				t.Errorf("job %q step %q hardcodes the declared set; reference $DECLARED_APT_PACKAGES so the cache key and the install can never disagree", name, step.Name)
			}
		}
	}
}

// TestDeclaredDependencyProvisioningPrefersCachedArchives is the regression test
// for the reported shape: provisioning reached the apt mirrors unconditionally,
// so a mirror stall was a 12-minute step timeout and a full rerun cycle. The
// cached payloads must be installed first, with no network, and the mirrors must
// remain only as the miss/fallback path.
func TestDeclaredDependencyProvisioningPrefersCachedArchives(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)
	job, ok := workflow.Jobs["integration"]
	if !ok {
		t.Fatal("CI workflow has no integration job")
	}

	restore := job.step(t, "Restore declared-dependency apt archives")
	if !strings.HasPrefix(restore.Uses, "actions/cache/restore@") {
		t.Errorf("declared-dependency restore step uses %q, want actions/cache/restore@<pinned>", restore.Uses)
	}
	if restore.ID != declaredAptCacheStep {
		t.Errorf("declared-dependency restore step id = %q, want %q", restore.ID, declaredAptCacheStep)
	}
	if got := restore.with("path"); got != declaredAptArchiveDir {
		t.Errorf("restore path = %q, want %q", got, declaredAptArchiveDir)
	}
	if got := restore.with("key"); got != declaredAptKeyExpr {
		t.Errorf("restore key = %q, want the derived key %q", got, declaredAptKeyExpr)
	}
	if got := restore.with("restore-keys"); got != "" {
		t.Errorf("restore-keys = %q; the declared-dependency cache must be exact-key only — a prefix match hands the job the .deb set for a different declared list and installs it silently", got)
	}

	const provision = "Provision declared dependencies (bash, bwrap, dirname, head, java, mkdir, mvn, sh, sleep, yes)"
	if job.stepIndex(restore.Name) > job.stepIndex(provision) {
		t.Fatalf("the archive cache must be restored before %q runs", provision)
	}

	script := job.step(t, provision).Run
	dpkg := strings.Index(script, "dpkg --install")
	if dpkg < 0 {
		t.Fatal("provisioning has no cached-archive install path; a cache the step never reads is not a fix")
	}
	apt := strings.Index(script, "apt-get")
	if apt < 0 {
		t.Fatal("provisioning must keep the apt path as the cache-miss fallback")
	}
	if dpkg > apt {
		t.Error("provisioning reaches apt-get before installing the cached archives; the cached path must come first so the common case does no network at all")
	}
	if !strings.Contains(script, `Dir::Cache::archives="$DECLARED_APT_ARCHIVE_DIR"`) {
		t.Error("the fallback apt-get must download into DECLARED_APT_ARCHIVE_DIR, otherwise a cache-miss run has nothing to publish and the next run misses too")
	}

	save := job.step(t, "Save declared-dependency apt archives")
	if !strings.HasPrefix(save.Uses, "actions/cache/save@") {
		t.Errorf("declared-dependency save step uses %q, want actions/cache/save@<pinned>", save.Uses)
	}
	if got := save.with("key"); got != declaredAptKeyExpr {
		t.Errorf("save key = %q, want %q — writing under a different key than the restore reads is a cache that never hits", got, declaredAptKeyExpr)
	}
	if got := save.with("path"); got != declaredAptArchiveDir {
		t.Errorf("save path = %q, want %q", got, declaredAptArchiveDir)
	}
	if !strings.Contains(save.If, "success()") {
		t.Errorf("save condition = %q, must include success(): a provisioning run that failed after three bounded attempts leaves a partial archive set, and publishing that under the shared key poisons every later run", save.If)
	}
	if !strings.Contains(save.If, "cache-hit") {
		t.Errorf("save condition = %q, must skip the upload when the restore already hit", save.If)
	}
}

// TestDeclaredDependencyCacheHasDefaultBranchWriter guards the failure mode that
// makes this cache look like it works while doing nothing. GitHub scopes a cache
// entry to the ref that wrote it: an entry written by a pull-request run is
// readable only by that pull request, and only entries written on the default
// branch are visible to every branch. The consuming job is PR-only, so without a
// writer on main every pull request's first run still provisions from the
// mirrors — the exact timeout class this cache exists to remove.
func TestDeclaredDependencyCacheHasDefaultBranchWriter(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)

	consumer, ok := workflow.Jobs["integration"]
	if !ok {
		t.Fatal("CI workflow has no integration job")
	}
	if !strings.Contains(consumer.If, "github.event_name != 'push'") {
		t.Fatal("integration is expected to stay pull-request-only; if that changed, this cache no longer needs a separate default-branch writer and this test should be revisited")
	}

	warm, ok := workflow.Jobs[declaredCacheWarmJob]
	if !ok {
		t.Fatalf("CI workflow has no %q job: the declared-dependency cache would only ever be written on pull-request refs, where no other pull request can read it", declaredCacheWarmJob)
	}
	if !strings.Contains(warm.If, "github.event_name == 'push'") {
		t.Errorf("%s condition = %q, want it to run on pushes to main so the entry lands on the default branch", declaredCacheWarmJob, warm.If)
	}
	if !warm.ContinueOnError {
		t.Errorf("%s must be continue-on-error: warming is an optimisation and every consumer falls back to the mirrors, so a mirror outage must not redden main", declaredCacheWarmJob)
	}

	save := warm.step(t, "Save declared-dependency apt archives")
	if got := save.with("key"); got != declaredAptKeyExpr {
		t.Errorf("%s save key = %q, want %q", declaredCacheWarmJob, got, declaredAptKeyExpr)
	}
	if got := save.with("path"); got != declaredAptArchiveDir {
		t.Errorf("%s save path = %q, want %q", declaredCacheWarmJob, got, declaredAptArchiveDir)
	}

	// It gates nothing, in either direction.
	for _, gate := range []string{"required-ci", "escalate-main-failure"} {
		job, ok := workflow.Jobs[gate]
		if !ok {
			t.Fatalf("CI workflow has no %q job", gate)
		}
		if slices.Contains(job.Needs, declaredCacheWarmJob) {
			t.Errorf("%s must not depend on %s; cache warming is an optimisation, not a gate", gate, declaredCacheWarmJob)
		}
	}
}

// TestDeclaredDependencyCacheKeyDerivationIsShared is the drift guard. The two
// jobs derive the key with their own copy of the same shell; if those copies
// diverge the warm job publishes an entry the consumer never asks for, and the
// only symptom is that CI is slow again.
func TestDeclaredDependencyCacheKeyDerivationIsShared(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)

	const name = "Resolve declared-dependency cache key"
	derivations := map[string][]string{}
	for _, job := range []string{"integration", declaredCacheWarmJob} {
		current, ok := workflow.Jobs[job]
		if !ok {
			t.Fatalf("CI workflow has no %q job", job)
		}
		step := current.step(t, name)
		if step.ID != declaredAptKeyStepID {
			t.Errorf("%s step %q id = %q, want %q", job, name, step.ID, declaredAptKeyStepID)
		}
		derivations[job] = significantLines(step.Run)
	}

	if !slices.Equal(derivations["integration"], derivations[declaredCacheWarmJob]) {
		t.Errorf("declared-dependency cache key derivation has drifted between the consumer and the warm job.\nintegration:\n  %s\n%s:\n  %s",
			strings.Join(derivations["integration"], "\n  "),
			declaredCacheWarmJob,
			strings.Join(derivations[declaredCacheWarmJob], "\n  "))
	}

	derivation := strings.Join(derivations["integration"], "\n")
	if !strings.Contains(derivation, "DECLARED_APT_PACKAGES") {
		t.Error("the cache key must be a function of the declared set, so that changing the set — and only changing the set — reopens the network")
	}
	if !strings.Contains(derivation, "ImageOS") {
		t.Error("the cache key must include the runner image: a .deb resolved against one Ubuntu release is not interchangeable with another")
	}
	if !strings.Contains(derivation, "GITHUB_OUTPUT") {
		t.Error("the derivation must publish the key as a step output for the cache steps to consume")
	}
}

// TestJavaFixtureMavenRepositoryIsWarmedOnMain covers the other half of the
// provisioned set. setup-java keys its Maven cache on runner OS/arch plus the pom
// hash, so the warm job only lands on the entry the integration job looks for if
// it configures setup-java identically against the same checked-out pom.
func TestJavaFixtureMavenRepositoryIsWarmedOnMain(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)

	consumer := workflow.Jobs["integration"].stepUsing(t, "actions/setup-java@")
	warm := workflow.Jobs[declaredCacheWarmJob].stepUsing(t, "actions/setup-java@")

	if consumer.Uses != warm.Uses {
		t.Errorf("setup-java is pinned to %q in integration but %q in %s; a different action version can change the cache key derivation", consumer.Uses, warm.Uses, declaredCacheWarmJob)
	}
	for _, key := range []string{"distribution", "java-version", "cache"} {
		if got, want := warm.with(key), consumer.with(key); got != want {
			t.Errorf("%s setup-java %s = %q, want %q to match the consumer's Maven cache key", declaredCacheWarmJob, key, got, want)
		}
	}
	if consumer.with("cache") != "maven" {
		t.Errorf("integration setup-java cache = %q, want maven", consumer.with("cache"))
	}

	warmStep := workflow.Jobs[declaredCacheWarmJob].step(t, "Warm the Maven repository for the Java fixture gaggle")
	pom := filepath.Join("test", "e2e", "testdata", "javaservice", "pom.xml")
	if !strings.Contains(warmStep.Run, filepath.ToSlash(pom)) {
		t.Errorf("the Maven warm must resolve against %s, the pom the integration tier's fixture gaggle builds", filepath.ToSlash(pom))
	}
	if _, err := os.Stat(filepath.Join(moduleRoot(t), pom)); err != nil {
		t.Errorf("the warmed pom does not exist: %v", err)
	}
}
