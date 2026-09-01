package vnext

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

const featureRegistrySnapshotProgram = `package main

import (
	"encoding/json"
	"os"

	"github.com/goobers/goobers/internal/workflow"
)

func main() {
	if err := json.NewEncoder(os.Stdout).Encode(workflow.AllFeatures()); err != nil {
		panic(err)
	}
}
`

func TestFeatureRegistryAgainstLatestRelease(t *testing.T) {
	root := strings.TrimSpace(runCommand(t, "", "git", "rev-parse", "--show-toplevel"))
	released, tag := loadLatestReleasedFeatureRegistry(t, root)

	if _, err := newFeatureRegistryAgainstReleased(released, AllFeatures()); err != nil {
		t.Fatalf("current feature registry violates compatibility with %s: %v", tag, err)
	}
}

func TestLatestReleasedFeatureRegistryComesFromTag(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module github.com/goobers/goobers\n\ngo 1.26\n")
	writeFixtureFeatureRegistry(t, root, SupportGA, "v1.1.0", []SupportTransition{
		{Level: SupportPreview, SinceVersion: "dev"},
		{Level: SupportGA, SinceVersion: "v1.1.0"},
	})
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")
	runGit(t, root, "config", "commit.gpgSign", "false")
	runGit(t, root, "config", "tag.gpgSign", "false")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-q", "-m", "release ga feature")
	runGit(t, root, "tag", "v1.1.0")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, root, "init", "--bare", "-q", remote)
	runGit(t, root, "remote", "add", "origin", remote)
	runGit(t, root, "push", "-q", "origin", "HEAD:refs/heads/main", "--tags")

	fabricatedHistory := []SupportTransition{
		{Level: SupportPreview, SinceVersion: "dev"},
		{Level: SupportGA, SinceVersion: "v1.1.0"},
		{Level: SupportDeprecated, SinceVersion: "v1.2.0"},
		{Level: SupportRemoved, SinceVersion: "v1.3.0"},
	}
	writeFixtureFeatureRegistry(t, root, SupportRemoved, "v1.3.0", fabricatedHistory)
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-q", "-m", "fabricate deprecation and removal")
	runGit(t, root, "tag", "v9.0.0")

	released, tag := loadLatestReleasedFeatureRegistry(t, root)
	if tag != "v1.1.0" {
		t.Fatalf("latest release tag = %q, want v1.1.0", tag)
	}
	previous, ok := released.Lookup("example.feature")
	if !ok {
		t.Fatal("tagged feature is missing from the released registry")
	}
	if previous.Level != SupportGA {
		t.Fatalf("released feature level = %q, want tagged level %q", previous.Level, SupportGA)
	}

	removed := Feature{
		ID:                    "example.feature",
		Level:                 SupportRemoved,
		SinceVersion:          "v1.3.0",
		LastSupportingVersion: "v1.2.0",
		History:               fabricatedHistory,
	}
	if _, err := newFeatureRegistryAgainstReleased(released, []Feature{removed}); err == nil ||
		!strings.Contains(err.Error(), "must be deprecated in the latest released registry") {
		t.Fatalf("same-change deprecation and removal error = %v, want tagged-release failure", err)
	}
}

// TestReleaseBaselineGateFailsLoudlyWithoutTag proves the empty-baseline guard
// in loadLatestReleasedFeatureRegistry actually fires. It re-runs the real gate
// in a subprocess whose repository origin has no version-shaped tag — the exact
// shape of the 8-day window in which v0.1.0's tag was unreachable from main
// until #2924's ls-remote fix. Before the guard, that gate compared against an
// empty registry and passed vacuously.
func TestReleaseBaselineGateFailsLoudlyWithoutTag(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "README.md"), "fixture without releases\n")
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")
	runGit(t, root, "config", "commit.gpgSign", "false")
	runGit(t, root, "config", "tag.gpgSign", "false")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-q", "-m", "fixture without releases")
	// A tag that is not version-shaped must not count as a baseline either.
	runGit(t, root, "tag", "not-a-release")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, root, "init", "--bare", "-q", remote)
	runGit(t, root, "remote", "add", "origin", remote)
	runGit(t, root, "push", "-q", "origin", "HEAD:refs/heads/main", "--tags")

	output, err := runTestBinary(t, root, "^TestFeatureRegistryAgainstLatestRelease$")
	if err == nil {
		t.Fatalf("gate passed with no release baseline; it must fail loudly:\n%s", output)
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("re-run gate without baseline: %v: %s", err, output)
	}
	for _, want := range []string{
		"no release baseline",
		"nothing to compare against",
		"explicit opt-in",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("gate failure does not explain %q:\n%s", want, output)
		}
	}
}

// TestReleaseBaselineTagIsEchoed asserts the gate names the release it compared
// against in its log. A green gate that never says which baseline it used is
// the other half of how #2924's vacuous pass went unnoticed.
func TestReleaseBaselineTagIsEchoed(t *testing.T) {
	output, err := runTestBinary(t, "", "^TestLatestReleasedFeatureRegistryComesFromTag$")
	if err != nil {
		t.Fatalf("re-run fixture-tag test: %v:\n%s", err, output)
	}
	if !strings.Contains(output, "feature registry baseline: release v1.1.0") {
		t.Fatalf("baseline tag is not echoed in the test log:\n%s", output)
	}
}

func loadLatestReleasedFeatureRegistry(t *testing.T, repository string) (FeatureRegistry, string) {
	t.Helper()
	tag, revision := latestReleaseTag(t, repository)
	if tag == "" {
		// Comparing against an empty registry makes this gate pass vacuously.
		// That exact failure mode was live for 8 days while v0.1.0's tag was
		// unreachable from main (#2924) and nothing reported the guard was
		// off. v0.1.0 exists on origin, so an empty result here is always a
		// defect, never a fresh-repository default.
		t.Fatalf("no release baseline: git ls-remote found no version-shaped tag on origin of %s; "+
			"the compatibility gate has nothing to compare against and would pass vacuously; "+
			"a repository with genuinely no releases yet must make that an explicit opt-in, not a silent default",
			repository)
	}
	// Name the baseline so CI output shows which release the gate compared
	// against (asserted by TestReleaseBaselineTagIsEchoed).
	t.Logf("feature registry baseline: release %s (%s)", tag, revision)

	releaseTree := filepath.Join(t.TempDir(), "release")
	runGit(t, repository, "worktree", "add", "--detach", "-q", releaseTree, revision)
	t.Cleanup(func() {
		if output, err := testgit.Command("-C", repository, "worktree", "remove", "--force", releaseTree).CombinedOutput(); err != nil {
			t.Errorf("remove release worktree: %v: %s", err, strings.TrimSpace(string(output)))
		}
	})

	snapshotFile := filepath.Join(releaseTree, "feature_registry_snapshot.go")
	writeFile(t, snapshotFile, featureRegistrySnapshotProgram)
	defer func() {
		if err := os.Remove(snapshotFile); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove feature registry snapshot program: %v", err)
		}
	}()

	goCommand := os.Getenv("GO")
	if goCommand == "" {
		goCommand = "go"
	}
	output, err := runCommandAllowingFailure(t, releaseTree, goCommand, "run", "./feature_registry_snapshot.go")
	if err != nil {
		if offlineModuleFetchRefused(output) {
			// The gate is not being waived, it cannot be run here: this
			// program is compiled INSIDE the release tree, so it needs that
			// release's module graph, and a module cache populated for the
			// current tree does not contain it. #4145 - the cluster's runner
			// image sets GOPROXY=off behind a deny-first egress proxy, so
			// every in-cluster `make ci` reported a compatibility violation
			// that did not exist, and the runner charged it to whatever item
			// happened to be running. Every other build failure below stays a
			// failure, and CI with a reachable proxy still enforces this on
			// every PR - the #2924 rule that this gate must never pass
			// vacuously is unchanged.
			t.Skipf("release %s (%s) cannot be built here: its module graph is "+
				"absent from the local module cache and module downloads are "+
				"refused (GOPROXY=%q). The compatibility gate is enforced where "+
				"modules are fetchable; it cannot be evaluated offline: %s",
				tag, revision, os.Getenv("GOPROXY"), strings.TrimSpace(output))
		}
		t.Fatalf("%s run ./feature_registry_snapshot.go: %v: %s",
			goCommand, err, strings.TrimSpace(output))
	}
	var features []Feature
	if err := json.Unmarshal([]byte(output), &features); err != nil {
		t.Fatalf("decode feature registry from release %s: %v", tag, err)
	}
	features, err = FeaturesAtDSLVersion(features, DSLVersion)
	if err != nil {
		t.Fatalf("project feature registry from release %s to DSL %s: %v", tag, DSLVersion, err)
	}
	registry, err := NewFeatureRegistry(features)
	if err != nil {
		t.Fatalf("feature registry from release %s is invalid: %v", tag, err)
	}
	return registry, tag
}

func latestReleaseTag(t *testing.T, repository string) (string, string) {
	t.Helper()
	output := runGit(t, repository, "ls-remote", "--tags", "--refs", "origin")
	var latestTag, latestRevision string
	var latestVersion releaseVersion
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		tag := strings.TrimPrefix(fields[1], "refs/tags/")
		version, err := parseReleaseVersion(tag, false)
		if err != nil {
			continue
		}
		if latestTag == "" || compareReleaseVersions(version, latestVersion) > 0 {
			latestTag = tag
			latestRevision = fields[0]
			latestVersion = version
		}
	}
	return latestTag, latestRevision
}

func writeFixtureFeatureRegistry(
	t *testing.T,
	root string,
	level SupportLevel,
	sinceVersion string,
	history []SupportTransition,
) {
	t.Helper()
	historyJSON, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf(`package workflow

type SupportTransition struct {
	Level        string `+"`json:\"level\"`"+`
	SinceVersion string `+"`json:\"sinceVersion\"`"+`
}

type DSLFeatureSupport struct {
	Version string `+"`json:\"version\"`"+`
	Level   string `+"`json:\"level\"`"+`
}

type Feature struct {
	ID           string              `+"`json:\"id\"`"+`
	Level        string              `+"`json:\"level\"`"+`
	SinceVersion string              `+"`json:\"sinceVersion\"`"+`
	DSLVersions  []DSLFeatureSupport `+"`json:\"dslVersions\"`"+`
	History      []SupportTransition `+"`json:\"history\"`"+`
}

func AllFeatures() []Feature {
	var history []SupportTransition
	if err := json.Unmarshal([]byte(%q), &history); err != nil {
		panic(err)
	}
	return []Feature{{
		ID:           "example.feature",
		Level:        %q,
		SinceVersion: %q,
		DSLVersions: []DSLFeatureSupport{{
			Version: %q,
			Level:   %q,
		}},
		History:      history,
	}}
}
`, historyJSON, level, sinceVersion, DSLVersion, level)
	source = "package workflow\n\nimport \"encoding/json\"\n\n" + strings.TrimPrefix(source, "package workflow\n\n")
	writeFile(t, filepath.Join(root, "internal", "workflow", "features.go"), source)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	return runCommand(t, "", "git", append([]string{"-C", repository}, args...)...)
}

// The classifier must be narrow in both directions: an offline refusal is not
// a compatibility violation, and a real build failure is not an excuse to skip
// the gate (#2924, #4145).
func TestOfflineModuleFetchRefused(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "GOPROXY off",
			output: "go: downloading k8s.io/apimachinery v0.36.3\napi/v1alpha1/goober_types.go:4:2: module lookup disabled by GOPROXY=off",
			want:   true,
		},
		{
			name:   "toolchain not cached",
			output: "go: downloading go1.26.6 (linux/amd64)\ngo: download go1.26.6 for linux/amd64: toolchain not available",
			want:   true,
		},
		{
			name:   "proxy unreachable",
			output: "go: downloading k8s.io/apimachinery v0.36.3\ndial tcp 142.250.1.141:443: connect: connection refused",
			want:   true,
		},
		{
			name:   "proxy denies the fetch",
			output: "go: downloading k8s.io/apimachinery v0.36.3\nreading https://proxy.example/@v/list: 403 Forbidden",
			want:   true,
		},
		{
			name:   "compile error in the release tree",
			output: "./feature_registry_snapshot.go:9:14: undefined: workflow.AllFeatures",
			want:   false,
		},
		{
			name:   "a downloaded module that then failed to build",
			output: "go: downloading k8s.io/apimachinery v0.36.3\n./x.go:3:2: cannot use n (variable of type int) as string value",
			want:   false,
		},
		{
			name:   "the snapshot program panicked",
			output: "panic: json: unsupported type\n\ngoroutine 1 [running]:",
			want:   false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := offlineModuleFetchRefused(testCase.output); got != testCase.want {
				t.Errorf("offlineModuleFetchRefused(%q) = %v, want %v",
					testCase.output, got, testCase.want)
			}
		})
	}
}

// offlineModuleFetchRefused reports whether output is `go` declining to reach
// a module proxy, rather than a defect in the code it was asked to build.
// The spellings differ by cause: an explicitly disabled proxy states so, a
// pinned toolchain that is not cached says the toolchain is unavailable, and
// a proxy that is merely unreachable surfaces as a transport error under a
// `go: downloading` line.
func offlineModuleFetchRefused(output string) bool {
	lowered := strings.ToLower(output)
	// An explicitly disabled proxy says so, and a toolchain the release pins
	// but this cache does not hold is the same refusal one layer down - `go`
	// fetches toolchains through the module proxy too.
	if strings.Contains(lowered, "module lookup disabled") ||
		strings.Contains(lowered, "toolchain not available") {
		return true
	}
	if !strings.Contains(lowered, "go: downloading") {
		return false
	}
	for _, marker := range []string{
		"dial tcp",
		"connection refused",
		"i/o timeout",
		"no such host",
		"tls handshake",
		"proxy error",
		"403 forbidden",
		"407 proxy",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// runCommandAllowingFailure is runCommand for a command whose failure the
// caller must classify rather than report. runCommand cannot serve: it calls
// t.Fatalf, which ends the test before the output can be read.
func runCommandAllowingFailure(t *testing.T, directory, name string, args ...string) (string, error) {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	return string(output), err
}

func runCommand(t *testing.T, directory, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	if name == "git" {
		command = testgit.Command(args...)
		command.Env = append(command.Env,
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=core.autocrlf",
			"GIT_CONFIG_VALUE_0=false",
			"GIT_CONFIG_KEY_1=core.safecrlf",
			"GIT_CONFIG_VALUE_1=false",
		)
	}
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output)
}

// runTestBinary re-executes this test binary against a single test so a test
// can observe another test failing or logging: t.Fatalf and t.Logf cannot be
// observed in-process. An empty directory inherits the parent's working
// directory.
func runTestBinary(t *testing.T, directory, pattern string) (string, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run="+pattern, "-test.v")
	command.Dir = directory
	output, err := command.CombinedOutput()
	return string(output), err
}
