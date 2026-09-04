package supportmatrix

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

const supportMatrixSnapshotProgram = `package main

import (
	"encoding/json"
	"os"

	"github.com/goobers/goobers/internal/supportmatrix"
)

func main() {
	if err := json.NewEncoder(os.Stdout).Encode(supportmatrix.GetDSL().Versions()); err != nil {
		panic(err)
	}
}
`

func TestDSLMatrixAgainstLatestRelease(t *testing.T) {
	root := strings.TrimSpace(runSupportCommand(t, "", "git", "rev-parse", "--show-toplevel"))
	released, tag, developmentReleases := loadLatestReleasedSupportMatrix(t, root)
	current := GetDSL()
	baseline := tag
	if baseline == "" {
		baseline = initialSupportVersion
	}

	if err := ValidateSupportPolicy(current); err != nil {
		t.Fatalf("compiled-in DSL support matrix violates support policy: %v", err)
	}
	if err := validateSupportMatrixEvolution(released, current, baseline, developmentReleases); err != nil {
		if tag == "" {
			t.Fatalf("current DSL support matrix violates the pre-release evolution policy: %v", err)
		}
		t.Fatalf("current DSL support matrix violates compatibility with %s: %v", tag, err)
	}
}

func TestDSLMatrixAgainstNextReleases(t *testing.T) {
	root := strings.TrimSpace(runSupportCommand(t, "", "git", "rev-parse", "--show-toplevel"))
	released, latestTag, _ := loadLatestReleasedSupportMatrix(t, root)
	firstTag, _, _ := supportReleaseTagRange(t, root)
	var latest releaseVersion
	if latestTag != "" {
		var err error
		latest, err = parseSupportReleaseVersion(latestTag, false)
		if err != nil {
			t.Fatalf("parse latest release tag %s: %v", latestTag, err)
		}
	}
	var firstRelease releaseVersion
	if firstTag != "" {
		var err error
		firstRelease, err = parseSupportReleaseVersion(firstTag, false)
		if err != nil {
			t.Fatalf("parse first release tag %s: %v", firstTag, err)
		}
	}
	nextPatch := latest
	nextPatch.patch++
	nextMinor := releaseVersion{major: latest.major, minor: latest.minor + 1}

	for _, candidate := range []releaseVersion{nextPatch, nextMinor} {
		t.Run(candidate.String(), func(t *testing.T) {
			releaseAnchor := firstRelease
			if firstTag == "" {
				releaseAnchor = candidate
			}
			if err := validateSupportMatrixAfterTag(
				released,
				GetDSL(),
				latestTag,
				candidate,
				releaseAnchor,
			); err != nil {
				t.Fatalf("compiled-in DSL support matrix would fail after cutting %s: %v", candidate.String(), err)
			}
		})
	}
}

func TestPreTagSimulationRejectsOffByOneSupportDeadline(t *testing.T) {
	matrix := SupportMatrix{
		"1.0": {
			Level:            LevelDeprecated,
			UnsupportedAfter: "v1.3.0",
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
				{Level: LevelDeprecated, SinceVersion: "v1.1.0"},
			},
		},
		"2.0": {
			Level: LevelSupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
			},
		},
	}
	firstRelease := releaseVersion{major: 1, minor: 1}
	nextRelease := releaseVersion{major: 1, minor: 2}

	err := validateSupportMatrixAfterTag(SupportMatrix{}, matrix, "", nextRelease, firstRelease)
	if err == nil || !strings.Contains(err.Error(), "fewer than 3 minor releases") {
		t.Fatalf("off-by-one support deadline error = %v, want three-minor support-window failure", err)
	}
}

// validateSupportMatrixAfterTag simulates cutting release from candidate. It
// validates candidate against the matrix released in latestRelease first — the
// baseline that gives validateSupportMatrixEvolution's release-sensitive loop
// real input, without which every new-transition rule iterates an empty slice
// (#4215) — and then against the post-tag world, where candidate is itself the
// released matrix and the support window must hold with release anchored.
func validateSupportMatrixAfterTag(
	released, candidate SupportMatrix,
	latestRelease string,
	release releaseVersion,
	firstRelease releaseVersion,
) error {
	developmentReleases := make(map[string]releaseVersion)
	for _, version := range candidate.Versions() {
		if len(version.History) > 0 && version.History[0].SinceVersion == initialSupportVersion {
			developmentReleases[version.Version] = firstRelease
		}
	}
	if latestRelease == "" {
		latestRelease = initialSupportVersion
	}
	if err := validateSupportMatrixEvolution(released, candidate, latestRelease, developmentReleases); err != nil {
		return err
	}
	return validateSupportMatrixEvolution(candidate, candidate, release.String(), developmentReleases)
}

// betaTwoSupportMatrix reconstructs the DSL support matrix shipped in
// v0.4.0-beta.1 and v0.4.0-beta.2: DSL 1.4 declared unsupported with the
// transition dated v0.5.0, a release that does not exist yet.
func betaTwoSupportMatrix() SupportMatrix {
	return SupportMatrix{
		"1.4": {
			Level:       LevelUnsupported,
			Replacement: "2.0",
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
				{Level: LevelDeprecated, SinceVersion: "v0.1.0"},
				{Level: LevelUnsupported, SinceVersion: "v0.5.0"},
			},
		},
		"2.0": {
			Level: LevelSupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
			},
		},
	}
}

// lastReleasedSupportMatrix reconstructs the matrix published in v0.3.3, the
// last release before the beta line.
func lastReleasedSupportMatrix() SupportMatrix {
	return SupportMatrix{
		"1.4": {
			Level:            LevelDeprecated,
			Replacement:      "2.0",
			UnsupportedAfter: "v0.5.0",
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
				{Level: LevelDeprecated, SinceVersion: "v0.1.0"},
			},
		},
		"2.0": {
			Level: LevelSupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
			},
		},
	}
}

func TestReleaseLevelRefusesLevelAheadOfItsTransition(t *testing.T) {
	candidate := betaTwoSupportMatrix()
	released := lastReleasedSupportMatrix()
	firstRelease := releaseVersion{major: 0, minor: 1}
	cut := releaseVersion{major: 0, minor: 4}

	if err := validateSupportMatrixAfterTag(released, candidate, "v0.3.3", cut, firstRelease); err != nil {
		t.Fatalf("evolution rules alone reject the beta.2 matrix (%v); the release-level check is what must catch it", err)
	}
	err := ValidateSupportPolicyForRelease(candidate, cut.String())
	if err == nil || !strings.Contains(err.Error(), `only reaches "deprecated"`) {
		t.Fatalf("release-level error = %v, want DSL 1.4 refused for declaring unsupported at v0.4.0", err)
	}
	if err := ValidateSupportPolicyForRelease(candidate, "v0.5.0"); err != nil {
		t.Fatalf("beta.2 matrix must be valid in the release its transition names: %v", err)
	}
}

func TestReleaseLevelRequiresATransitionAtOrBeforeTheRelease(t *testing.T) {
	matrix := SupportMatrix{
		"3.0": {
			Level: LevelPreview,
			History: []SupportTransition{
				{Level: LevelPreview, SinceVersion: "v0.4.0"},
			},
		},
	}
	err := ValidateSupportPolicyForRelease(matrix, "v0.3.3")
	if err == nil || !strings.Contains(err.Error(), "after the release") {
		t.Fatalf("release-level error = %v, want a not-yet-in-lifecycle failure", err)
	}
	if err := ValidateSupportPolicyForRelease(matrix, "v0.4.0"); err != nil {
		t.Fatalf("preview declared in the release being built must be accepted: %v", err)
	}
}

func TestPreTagSimulationRefusesTransitionAtTheLatestRelease(t *testing.T) {
	candidate := betaTwoSupportMatrix()
	candidate["3.0"] = VersionSupport{
		Level: LevelPreview,
		History: []SupportTransition{
			{Level: LevelPreview, SinceVersion: "v0.3.3"},
		},
	}
	released := lastReleasedSupportMatrix()
	firstRelease := releaseVersion{major: 0, minor: 1}
	cut := releaseVersion{major: 0, minor: 4}

	err := validateSupportMatrixAfterTag(released, candidate, "v0.3.3", cut, firstRelease)
	if err == nil || !strings.Contains(err.Error(), "must be later than latest release") {
		t.Fatalf("pre-tag simulation error = %v, want the later-than-latest-release failure", err)
	}
}

func TestEvolutionKeepsPublishedUnsupportedAfter(t *testing.T) {
	released := lastReleasedSupportMatrix()
	firstRelease := releaseVersion{major: 0, minor: 1}
	developmentReleases := map[string]releaseVersion{"1.4": firstRelease, "2.0": firstRelease}

	pulledIn := betaTwoSupportMatrix()
	version := pulledIn["1.4"]
	version.History[len(version.History)-1].SinceVersion = "v0.4.0"
	pulledIn["1.4"] = version
	err := validateSupportMatrixEvolution(released, pulledIn, "v0.3.3", developmentReleases)
	if err == nil || !strings.Contains(err.Error(), "before the unsupported-after release") {
		t.Fatalf("pulled-in unsupported release error = %v, want the published-deadline failure", err)
	}

	dropped := lastReleasedSupportMatrix()
	stillDeprecated := dropped["1.4"]
	stillDeprecated.UnsupportedAfter = "v0.4.0"
	dropped["1.4"] = stillDeprecated
	err = validateSupportMatrixEvolution(released, dropped, "v0.3.3", developmentReleases)
	if err == nil || !strings.Contains(err.Error(), "moves its unsupported-after release earlier") {
		t.Fatalf("earlier unsupported-after error = %v, want the published-deadline failure", err)
	}

	if err := validateSupportMatrixEvolution(released, betaTwoSupportMatrix(), "v0.3.3", developmentReleases); err != nil {
		t.Fatalf("honoring the published unsupported-after release must be accepted: %v", err)
	}
}

func TestLatestReleasedSupportMatrixComesFromTag(t *testing.T) {
	root := t.TempDir()
	writeSupportFile(t, filepath.Join(root, "go.mod"), "module github.com/goobers/goobers\n\ngo 1.26\n")
	initialVersions := []Version{
		{
			Version: "1.0",
			Level:   LevelSupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
			},
		},
	}
	writeFixtureSupportMatrix(t, root, initialVersions)
	runSupportGit(t, root, "init", "-q")
	runSupportGit(t, root, "config", "user.email", "test@example.com")
	runSupportGit(t, root, "config", "user.name", "Test")
	runSupportGit(t, root, "config", "commit.gpgSign", "false")
	runSupportGit(t, root, "config", "tag.gpgSign", "false")
	runSupportGit(t, root, "add", ".")
	runSupportGit(t, root, "commit", "-q", "-m", "release initial supported matrix")
	runSupportGit(t, root, "tag", "v1.0.0")

	releasedVersions := append(initialVersions, Version{
		Version: "1.1",
		Level:   LevelSupported,
		History: []SupportTransition{
			{Level: LevelSupported, SinceVersion: "v1.1.0"},
		},
	})
	writeFixtureSupportMatrix(t, root, releasedVersions)
	runSupportGit(t, root, "add", ".")
	runSupportGit(t, root, "commit", "-q", "-m", "release second supported matrix")
	runSupportGit(t, root, "tag", "v1.1.0")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runSupportGit(t, root, "init", "--bare", "-q", remote)
	runSupportGit(t, root, "remote", "add", "origin", remote)
	runSupportGit(t, root, "push", "-q", "origin", "HEAD:refs/heads/main", "--tags")

	fabricatedVersions := []Version{
		{
			Version: "1.0",
			Level:   LevelUnsupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
				{Level: LevelDeprecated, SinceVersion: "v1.3.0"},
				{Level: LevelUnsupported, SinceVersion: "v1.4.0"},
			},
		},
		releasedVersions[1],
	}
	writeFixtureSupportMatrix(t, root, fabricatedVersions)
	runSupportGit(t, root, "add", ".")
	runSupportGit(t, root, "commit", "-q", "-m", "fabricate deprecation and unsupported transition")
	runSupportGit(t, root, "tag", "v9.0.0")

	released, tag, developmentReleases := loadLatestReleasedSupportMatrix(t, root)
	if tag != "v1.1.0" {
		t.Fatalf("latest release tag = %q, want v1.1.0", tag)
	}
	if firstRelease := developmentReleases["1.0"]; firstRelease.String() != "v1.0.0" {
		t.Fatalf("DSL 1.0 first release = %q, want v1.0.0", firstRelease.String())
	}
	previous, ok := released.Lookup("1.0")
	if !ok {
		t.Fatal("tagged DSL version is missing from the released support matrix")
	}
	if previous.Level != LevelSupported {
		t.Fatalf("released DSL version level = %q, want tagged level %q", previous.Level, LevelSupported)
	}

	current := versionsToSupportMatrix(fabricatedVersions)
	if err := ValidateSupportPolicy(current); err != nil {
		t.Fatalf("fabricated current matrix must satisfy its self-reported policy: %v", err)
	}
	if err := validateSupportMatrixEvolution(released, current, tag, developmentReleases); err == nil ||
		!strings.Contains(err.Error(), "must be deprecated in the latest released support matrix") {
		t.Fatalf("same-change deprecation and unsupported error = %v, want tagged-release failure", err)
	}
}

func loadLatestReleasedSupportMatrix(
	t *testing.T,
	repository string,
) (SupportMatrix, string, map[string]releaseVersion) {
	t.Helper()
	firstTag, latestTag, latestRevision := supportReleaseTagRange(t, repository)
	if latestTag == "" {
		return SupportMatrix{}, "", nil
	}

	latest := loadSupportMatrixAtRelease(t, repository, latestTag, latestRevision)
	firstRelease, err := parseSupportReleaseVersion(firstTag, false)
	if err != nil {
		t.Fatalf("parse first release tag %s: %v", firstTag, err)
	}
	developmentReleases := make(map[string]releaseVersion)
	// Evolution rejects new dev histories after a real release, so every dev
	// history retained by the latest matrix first appeared in the first tag.
	for _, version := range latest.Versions() {
		if len(version.History) > 0 && version.History[0].SinceVersion == initialSupportVersion {
			developmentReleases[version.Version] = firstRelease
		}
	}
	return latest, latestTag, developmentReleases
}

func loadSupportMatrixAtRelease(t *testing.T, repository, tag, revision string) SupportMatrix {
	t.Helper()
	releaseTree := filepath.Join(t.TempDir(), "release")
	runSupportGit(t, repository, "worktree", "add", "--detach", "-q", releaseTree, revision)
	t.Cleanup(func() {
		if output, err := testgit.Command("-C", repository, "worktree", "remove", "--force", releaseTree).CombinedOutput(); err != nil {
			t.Errorf("remove release worktree: %v: %s", err, strings.TrimSpace(string(output)))
		}
	})

	snapshotFile := filepath.Join(releaseTree, "support_matrix_snapshot.go")
	writeSupportFile(t, snapshotFile, supportMatrixSnapshotProgram)
	defer func() {
		if err := os.Remove(snapshotFile); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove support matrix snapshot program: %v", err)
		}
	}()

	goCommand := os.Getenv("GO")
	if goCommand == "" {
		goCommand = "go"
	}
	output := runSupportCommand(t, releaseTree, goCommand, "run", "./support_matrix_snapshot.go")
	var versions []Version
	if err := json.Unmarshal([]byte(output), &versions); err != nil {
		t.Fatalf("decode support matrix from release %s: %v", tag, err)
	}
	matrix := versionsToSupportMatrix(versions)
	if err := ValidateSupportPolicy(matrix); err != nil {
		t.Fatalf("support matrix from release %s is invalid: %v", tag, err)
	}
	return matrix
}

func supportReleaseTagRange(t *testing.T, repository string) (string, string, string) {
	t.Helper()
	output := runSupportGit(t, repository, "ls-remote", "--tags", "--refs", "origin")
	var firstTag, latestTag, latestRevision string
	var firstVersion, latestVersion releaseVersion
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		tag := strings.TrimPrefix(fields[1], "refs/tags/")
		version, err := parseSupportReleaseVersion(tag, false)
		if err != nil {
			continue
		}
		if firstTag == "" || compareReleaseVersions(version, firstVersion) < 0 {
			firstTag = tag
			firstVersion = version
		}
		if latestTag == "" || compareReleaseVersions(version, latestVersion) > 0 {
			latestTag = tag
			latestRevision = fields[0]
			latestVersion = version
		}
	}
	return firstTag, latestTag, latestRevision
}

func versionsToSupportMatrix(versions []Version) SupportMatrix {
	matrix := make(SupportMatrix, len(versions))
	for _, version := range versions {
		matrix[version.Version] = VersionSupport{
			Level:            version.Level,
			UnsupportedAfter: version.UnsupportedAfter,
			Replacement:      version.Replacement,
			History:          version.History,
		}
	}
	return matrix
}

func writeFixtureSupportMatrix(t *testing.T, root string, versions []Version) {
	t.Helper()
	versionsJSON, err := json.Marshal(versions)
	if err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf(`package supportmatrix

import "encoding/json"

type SupportTransition struct {
	Level        string `+"`json:\"level\"`"+`
	SinceVersion string `+"`json:\"sinceVersion\"`"+`
}

type Version struct {
	Version          string              `+"`json:\"version\"`"+`
	Level            string              `+"`json:\"level\"`"+`
	UnsupportedAfter string              `+"`json:\"unsupportedAfter,omitempty\"`"+`
	Replacement      string              `+"`json:\"replacement,omitempty\"`"+`
	History          []SupportTransition `+"`json:\"history\"`"+`
}

type SupportMatrix struct{}

func (SupportMatrix) Versions() []Version {
	var versions []Version
	if err := json.Unmarshal([]byte(%q), &versions); err != nil {
		panic(err)
	}
	return versions
}

func GetDSL() SupportMatrix {
	return SupportMatrix{}
}
`, versionsJSON)
	writeSupportFile(t, filepath.Join(root, "internal", "supportmatrix", "supportmatrix.go"), source)
}

func writeSupportFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runSupportGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	return runSupportCommand(t, "", "git", append([]string{"-C", repository}, args...)...)
}

func runSupportCommand(t *testing.T, directory, name string, args ...string) string {
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
