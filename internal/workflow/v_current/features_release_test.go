package vcurrent

import (
	"encoding/json"
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
		if tag == "" {
			t.Fatalf("current feature registry violates the pre-release compatibility policy: %v", err)
		}
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

func loadLatestReleasedFeatureRegistry(t *testing.T, repository string) (FeatureRegistry, string) {
	t.Helper()
	tag, revision := latestReleaseTag(t, repository)
	if tag == "" {
		registry, err := NewFeatureRegistry(nil)
		if err != nil {
			t.Fatal(err)
		}
		return registry, ""
	}

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
	output := runCommand(t, releaseTree, goCommand, "run", "./feature_registry_snapshot.go")
	var features []Feature
	if err := json.Unmarshal([]byte(output), &features); err != nil {
		t.Fatalf("decode feature registry from release %s: %v", tag, err)
	}
	features, err := FeaturesAtDSLVersion(features, DSLVersion)
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
