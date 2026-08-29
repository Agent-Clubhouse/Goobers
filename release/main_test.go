package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "x", "y"); got != "x" {
		t.Errorf("firstNonEmpty = %q, want x", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty(empties) = %q, want empty", got)
	}
}

func TestParseFlagsExplicit(t *testing.T) {
	// Explicit values bypass the git-derived defaults, so this is hermetic.
	opts, err := parseFlags([]string{
		"-version", "v9.9.9", "-commit", "abc123", "-date", "2026-01-02T03:04:05Z",
		"-targets", "windows/amd64", "-output", "out",
		"-previous-features", "previous-features.json",
		"-previous-support-matrix", "previous-support.json",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if opts.version != "v9.9.9" || opts.commit != "abc123" || opts.date != "2026-01-02T03:04:05Z" {
		t.Errorf("metadata not honored: %+v", opts)
	}
	if opts.outDir != "out" || opts.previousFeatures != "previous-features.json" ||
		len(opts.targets) != 1 || opts.targets[0].String() != "windows/amd64" {
		t.Errorf("release options not honored: %+v", opts)
	}
	if opts.previousSupportMatrix != "previous-support.json" {
		t.Errorf("previous support matrix = %q", opts.previousSupportMatrix)
	}
	if !opts.checksums {
		t.Error("checksums should default true")
	}
}

func TestParseFlagsRequiresExplicitFeatureBaseline(t *testing.T) {
	metadata := []string{"-version", "v1.0.0", "-commit", "abc123", "-date", "2026-01-02T03:04:05Z"}
	if _, err := parseFlags(metadata, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "feature baseline required") {
		t.Fatalf("missing baseline error = %v", err)
	}
	args := append(append([]string(nil), metadata...), "-previous-features", "previous.json", "-first-feature-snapshot")
	if _, err := parseFlags(args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("conflicting baseline error = %v", err)
	}
	args = append(append([]string(nil), metadata...), "-previous-features", "previous.json")
	if _, err := parseFlags(args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "support-matrix baseline required") {
		t.Fatalf("missing support baseline error = %v", err)
	}
	args = append(append([]string(nil), metadata...), "-first-feature-snapshot", "-previous-support-matrix", "previous.json")
	if _, err := parseFlags(args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("conflicting support baseline error = %v", err)
	}
}

func TestParseFlagsBadTarget(t *testing.T) {
	if _, err := parseFlags([]string{"-targets", "not-a-target", "-first-feature-snapshot"}, &bytes.Buffer{}); err == nil {
		t.Error("parseFlags with a bad target should error")
	}
}

// TestRunEndToEnd exercises the whole pipeline — cross-compile, package,
// release metadata, and checksums — against a small in-module package (this
// release tool itself), so it stays fast and independent of the daemon's
// cross-compile state.
func TestRunEndToEnd(t *testing.T) {
	orig := buildPackage
	buildPackage = "./"
	defer func() { buildPackage = orig }()

	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"-version", "v1.2.3", "-commit", "deadbee", "-date", "2026-01-02T03:04:05Z",
		"-targets", "windows/amd64", "-output", out, "-first-feature-snapshot",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}

	// Archive present and correctly named.
	archive := filepath.Join(out, "goobers_v1.2.3_windows_amd64.zip")
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("expected archive %s: %v", archive, err)
	}
	// Zip contains the target binary and release-pinned onboarding docs.
	zr, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	archiveEntries := make(map[string]*zip.File, len(zr.File))
	for _, entry := range zr.File {
		archiveEntries[entry.Name] = entry
	}
	for _, name := range []string{
		"goobers.exe",
		"README.md",
		releaseDocsVersionFile,
		"docs/cli/README.md",
		"docs/completion/goobers.bash",
		"docs/completion/goobers.fish",
		"docs/completion/_goobers",
		"docs/guides/quickstart.md",
		"docs/guides/quickstart-linux.md",
		"docs/man/goobers.1",
		"onboarding/manifest.json",
		"onboarding/templates/canonical/README.md",
		"onboarding/templates/canonical/gaggles/acme-web/workflows/implementation.yaml",
		"onboarding/templates/quickstart@v1/gaggles/example/workflows/quickstart.yaml",
		"onboarding/samples/getting-started-task-api@1.0.0/sample.json",
		"onboarding/samples/getting-started-task-api@1.0.0/seed-issues.json",
	} {
		if archiveEntries[name] == nil {
			t.Errorf("release archive missing %s", name)
		}
	}
	marker, err := readZipEntry(archiveEntries[releaseDocsVersionFile])
	if err != nil {
		t.Fatalf("read %s: %v", releaseDocsVersionFile, err)
	}
	for _, want := range []string{"Goobers v1.2.3 documentation", "commit `deadbee`", "command registry"} {
		if !strings.Contains(string(marker), want) {
			t.Errorf("%s missing %q:\n%s", releaseDocsVersionFile, want, marker)
		}
	}
	readme, err := readZipEntry(archiveEntries["README.md"])
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	for _, want := range []string{
		"bundled with release `v1.2.3`",
		"goobers-v1.2.3 --version",
		"Linux or macOS with mock providers",
		"The release installer installs the binary and documentation only",
		"default `./goobers-instance`",
		"replace `./my-instance` with that same path",
		"quoting it if needed",
		"directly from an extracted archive instead",
		"replace `goobers-v1.2.3`\nbelow with `./goobers`",
		"goobers-v1.2.3 init --template=quickstart ./tutorial-instance",
		"goobers-v1.2.3 run " + instance.GuidedWorkflowImplementation + " ./my-instance",
		"[`config-examples/`](onboarding/templates/canonical/README.md)",
	} {
		if !strings.Contains(string(readme), want) {
			t.Errorf("README.md missing installed onboarding command %q:\n%s", want, readme)
		}
	}
	for _, stale := range []string{"curl -fsSL", "install.sh", "$HOME/.local/bin", "bin/goobers init"} {
		if strings.Contains(string(readme), stale) {
			t.Errorf("README.md retains pre-install command %q:\n%s", stale, readme)
		}
	}
	if strings.Contains(string(readme), "installer configured `./my-instance`") {
		t.Errorf("README.md claims the installer initialized the direct-archive example path:\n%s", readme)
	}
	if got := strings.Count(string(readme), "goobers-v1.2.3 init --guided ./my-instance"); got != 1 {
		t.Errorf("README.md guided init count = %d, want one direct-archive command:\n%s", got, readme)
	}
	assertSubstringsInOrder(
		t,
		"bundled README onboarding",
		string(readme),
		"goobers-v1.2.3 --version",
		"The release installer installs the binary and documentation only",
		"default `./goobers-instance`",
		"replace `./my-instance` with that same path",
		"quoting it if needed",
		"directly from an extracted archive instead",
		"goobers-v1.2.3 init --guided ./my-instance",
		"goobers-v1.2.3 run "+instance.GuidedWorkflowImplementation+" ./my-instance",
	)

	quickstart, err := readZipEntry(archiveEntries["docs/guides/quickstart.md"])
	if err != nil {
		t.Fatalf("read docs/guides/quickstart.md: %v", err)
	}
	for _, want := range []string{
		"bundled with release `v1.2.3`",
		"goobers-v1.2.3 --version",
		"goobers-v1.2.3 init --demo ./demo-instance",
		"goobers-v1.2.3 run demo ./demo-instance",
		"goobers-v1.2.3 trace <run-id> ./demo-instance",
		"goobers-v1.2.3 onboarding stub-sample",
		"--destination ./getting-started-task-api",
		"--json",
		"embeds the release-matched sample",
		"goobers-v1.2.3 connect <owner>/<repo> --seed ./tutorial-instance",
		"GOOBERS_GITHUB_TOKEN",
		"[`config-examples` reference layout](../../onboarding/templates/canonical/README.md)",
		"[`implementation` workflow](../../onboarding/templates/canonical/gaggles/acme-web/workflows/implementation.yaml)",
		"installs the binary and documentation only",
		"opted in with `--guided [instance-path]`",
		"default `./goobers-instance`",
		"replace `./my-instance` with that same path",
		"quoting it if needed",
		"goobers-v1.2.3 init --guided ./my-instance",
		"goobers-v1.2.3 init --template=quickstart ./tutorial-instance",
		"goobers-v1.2.3 run quickstart ./tutorial-instance",
		"goobers-v1.2.3 dashboard ./tutorial-instance",
		"goobers-v1.2.3 run " + instance.GuidedWorkflowImplementation + " ./my-instance",
	} {
		if !strings.Contains(string(quickstart), want) {
			t.Errorf("docs/guides/quickstart.md missing installed onboarding command %q:\n%s", want, quickstart)
		}
	}
	for _, stale := range []string{
		"go build -o bin/goobers",
		"bin/goobers init",
		"goobers init ./my-instance",
		"cp -R samples/getting-started-task-api",
		"default-implement",
	} {
		if strings.Contains(string(quickstart), stale) {
			t.Errorf("docs/guides/quickstart.md retains source-checkout command %q:\n%s", stale, quickstart)
		}
	}
	if strings.Contains(string(quickstart), "installer and bundled README already run guided setup") {
		t.Errorf("docs/guides/quickstart.md conflates installer and direct-archive paths:\n%s", quickstart)
	}
	assertSubstringsInOrder(
		t,
		"bundled quickstart direct onboarding",
		string(quickstart),
		"goobers-v1.2.3 --version",
		"goobers-v1.2.3 init --demo ./demo-instance",
		"goobers-v1.2.3 onboarding stub-sample",
		"default `./goobers-instance`",
		"replace `./my-instance` with that same path",
		"quoting it if needed",
		"goobers-v1.2.3 init --guided ./my-instance",
		"goobers validate --source-tree \"<config-source>\"",
		"goobers-v1.2.3 run "+instance.GuidedWorkflowImplementation+" ./my-instance",
	)
	linuxQuickstart, err := readZipEntry(archiveEntries["docs/guides/quickstart-linux.md"])
	if err != nil {
		t.Fatalf("read Linux quickstart: %v", err)
	}
	for _, want := range []string{
		"Use the `goobers` daemon bundled with release `v1.2.3`",
		"## 1. Install runtime prerequisites",
		"source-only Linux validation harness is not included in release archives",
		"## 2. Confirm the installed binary",
		"[canonical quickstart](quickstart.md#3-init---guided--configure-a-regular-instance)",
		"every tool used by your configured workflows",
		"bundled [Daemon supervision]",
	} {
		if !strings.Contains(string(linuxQuickstart), want) {
			t.Errorf("Linux quickstart missing %q:\n%s", want, linuxQuickstart)
		}
	}
	for _, stale := range []string{
		"go build -o bin/goobers",
		"go run ./test/linuxvalidate",
		"./cmd/goobers",
		"./test/linuxvalidate",
		"## 2. Build the binary",
		"../../go.mod",
		"../../CONTRIBUTING.md",
		"../../packaging/systemd/goobers.service",
		".github/workflows/ci.yml",
		"goobers init ./my-instance",
		"`make ci`",
		"`golangci-lint`",
	} {
		if strings.Contains(string(linuxQuickstart), stale) {
			t.Errorf("Linux quickstart retains source-checkout content %q:\n%s", stale, linuxQuickstart)
		}
	}
	// SHA256SUMS written and references the archive.
	sums, err := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("SHA256SUMS: %v", err)
	}

	if !strings.Contains(string(sums), "goobers_v1.2.3_windows_amd64.zip") {
		t.Errorf("SHA256SUMS missing the archive:\n%s", sums)
	}
	if !strings.Contains(string(sums), installScriptFile) {
		t.Errorf("SHA256SUMS missing the install helper:\n%s", sums)
	}
	onboardingName := onboardingArchiveName("v1.2.3")
	if !strings.Contains(string(sums), onboardingName) {
		t.Errorf("SHA256SUMS missing onboarding assets:\n%s", sums)
	}
	notes, err := os.ReadFile(filepath.Join(out, releaseNotesFile))
	if err != nil {
		t.Fatalf("%s: %v", releaseNotesFile, err)
	}
	for _, heading := range []string{"## DSL feature-support delta", "## DSL support-matrix delta"} {
		if !strings.Contains(string(notes), heading) {
			t.Errorf("release notes missing %q:\n%s", heading, notes)
		}
	}
	snapshot, err := readSupportSnapshot(filepath.Join(out, supportSnapshotFile))
	if err != nil {
		t.Fatalf("%s: %v", supportSnapshotFile, err)
	}
	if snapshot.Release != "v1.2.3" {
		t.Errorf("support snapshot release = %q", snapshot.Release)
	}
	if !strings.Contains(string(sums), featureSnapshotFile) {
		t.Errorf("SHA256SUMS missing the feature snapshot:\n%s", sums)
	}
	if !strings.Contains(string(sums), supportSnapshotFile) {
		t.Errorf("SHA256SUMS missing the support snapshot:\n%s", sums)
	}
	toolkitName := agentToolkitArchiveName("v1.2.3")
	if !strings.Contains(string(sums), toolkitName) {
		t.Errorf("SHA256SUMS missing the agent toolkit:\n%s", sums)
	}
	toolkitEntries := readAgentToolkitArchive(t, filepath.Join(out, toolkitName))
	var toolkitManifest agentToolkitManifest
	if err := json.Unmarshal(toolkitEntries["manifest.json"], &toolkitManifest); err != nil {
		t.Fatalf("decode agent toolkit manifest: %v", err)
	}
	if toolkitManifest.Producer != (agentToolkitProducer{Version: "v1.2.3", Commit: "deadbee"}) {
		t.Errorf("agent toolkit producer = %+v", toolkitManifest.Producer)
	}
	onboardingEntries := readAgentToolkitArchive(t, filepath.Join(out, onboardingName))
	for name, standalone := range onboardingEntries {
		platformEntry := archiveEntries["onboarding/"+name]
		platformData, err := readZipEntry(platformEntry)
		if err != nil {
			t.Errorf("platform archive onboarding entry %s: %v", name, err)
			continue
		}
		if !bytes.Equal(standalone, platformData) {
			t.Errorf("standalone onboarding entry %s differs from platform archive", name)
		}
	}
	// The intermediate binary was cleaned up, leaving only release assets.
	entries, _ := os.ReadDir(out)
	if len(entries) != 8 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dist has %v, want binary archive, agent toolkit, onboarding payload, installer, checksums, release notes, feature snapshot, and support snapshot", names)
	}
	for _, name := range []string{
		installScriptFile,
		releaseNotesFile,
		featureSnapshotFile,
		supportSnapshotFile,
		toolkitName,
		onboardingName,
	} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("missing release metadata %s: %v", name, err)
		}
	}
}

func assertSubstringsInOrder(t *testing.T, name, text string, wants ...string) {
	t.Helper()
	offset := 0
	for _, want := range wants {
		index := strings.Index(text[offset:], want)
		if index < 0 {
			t.Fatalf("%s does not contain %q after byte %d", name, want, offset)
		}
		offset += index + len(want)
	}
}

func readZipEntry(entry *zip.File) ([]byte, error) {
	if entry == nil {
		return nil, os.ErrNotExist
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(reader)
}

// TestRunSkipUnbuildable proves the skip path: an impossible target is skipped
// (not fatal) under -skip-unbuildable, and produces no partial artifacts.
func TestRunSkipUnbuildable(t *testing.T) {
	orig := buildPackage
	buildPackage = "./"
	defer func() { buildPackage = orig }()

	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"-targets", "windows/ppc64", // an unsupported GOOS/GOARCH pair: fails fast
		"-skip-unbuildable", "-output", out, "-first-feature-snapshot",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run with -skip-unbuildable should not fail: %v", err)
	}
	if !strings.Contains(stdout.String(), "skip") {
		t.Errorf("expected a skip notice, got:\n%s", stdout.String())
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Errorf("no artifacts should be produced when the only target is skipped, got %d", len(entries))
	}
}
