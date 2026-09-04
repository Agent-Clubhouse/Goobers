package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeExecutor struct {
	calls        []check
	outputs      map[string][]byte
	failCommands map[string]bool
	// transientFailures[label] is how many leading attempts fail before the
	// command succeeds, modelling a briefly unavailable remote service.
	transientFailures map[string]int
}

func (f *fakeExecutor) run(current check) ([]byte, error) {
	f.calls = append(f.calls, current)
	key := current.label
	if current.label == "" {
		key = strings.Join(append([]string{current.command}, current.args...), " ")
	}
	output := f.outputs[key]
	if f.failCommands[key] {
		return output, errors.New("command failed")
	}
	if f.transientFailures[key] > 0 {
		f.transientFailures[key]--
		return output, errors.New("command failed")
	}
	return output, nil
}

func TestChecksPreserveMergeGateOrder(t *testing.T) {
	t.Parallel()
	tools := toolchain{
		goCommand:       "custom-go",
		gofmtCommand:    "gofmt",
		gitCommand:      "git",
		npmCommand:      "npm",
		golangciCommand: "golangci-lint",
	}
	metadata := buildMetadata{version: "v1.2.3", commit: "abcdef0", date: "2026-07-20T12:00:00Z"}

	gotChecks := checks([]string{"config-sync", "goobers", "operator"}, tools, metadata, "linux", "")
	var got []string
	for _, current := range gotChecks {
		got = append(got, current.label)
	}
	want := []string{
		"fmt-check",
		"tidy-check",
		"no-phone-home",
		"stage-name-lint",
		"vet",
		"flake-policy",
		"design-doc-status",
		"markdown-links",
		"npm-registry",
		"go-toolchain",
		"build-config-sync",
		"portal-install",
		"portal-audit",
		"portal-playwright-install",
		"portal-build",
		"portal-embed-vet",
		"build-goobers",
		"validate-configs",
		"build-operator",
		"shipped-workflows",
		"schema-description-coverage",
		"test",
		"lint",
		"portal-test",
		"portal-deadcode",
		"portal-e2e",
		"portal-contract-generate",
		"portal-contract-diff",
		"portal-contract-typecheck",
		"portal-contract-test",
		"manifests-generate",
		"manifests-diff",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("check order = %q, want %q", got, want)
	}

	// Looked up by label rather than by position: this assertion previously
	// used a hardcoded index, which silently selected the wrong check as soon
	// as a new check was inserted earlier in the list (adding no-phone-home
	// shifted "test" from 12 to 13). A label lookup cannot drift that way.
	testCheck := checkByLabel(t, gotChecks, "test")
	wantEnv := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.fsync",
		"GIT_CONFIG_VALUE_0=none",
		"GOOBERS_DISABLE_FSYNC=1",
		"GOENV=off",
		"GOFLAGS=-mod=readonly",
		"GONOPROXY=none",
		"GONOSUMDB=none",
		"GOPRIVATE=",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		"GOVCS=*:off",
		"GOOBERS_SKIP_SHIPPED_WORKFLOW_CONTRACTS=1",
	}
	if !reflect.DeepEqual(testCheck.env, wantEnv) {
		t.Fatalf("test environment = %q, want %q", testCheck.env, wantEnv)
	}
	wantTestArgs := []string{
		"run", "./test/hermetic", "--go-command", "custom-go", "--",
		"-race", "-timeout", "30m", "-count=1", "-covermode=atomic", "-coverprofile=coverage.out", "./...",
	}
	if !reflect.DeepEqual(testCheck.args, wantTestArgs) {
		t.Fatalf("test arguments = %q, want %q", testCheck.args, wantTestArgs)
	}
	shippedCheck := checkByLabel(t, gotChecks, "shipped-workflows")
	// Plain `go test`, NOT routed through test/hermetic: a linked-in-isolation
	// git.exe cannot find its libexec helpers on Windows (see the comment at the
	// construction site, and PR #3461 where every contract failed at `git init`
	// on windows-latest).
	wantShippedArgs := []string{"test", "-race", "-timeout", "20m", "-count=1", "./test/shippedworkflows"}
	if shippedCheck.label != "shipped-workflows" ||
		!reflect.DeepEqual(shippedCheck.args, wantShippedArgs) {
		t.Fatalf("shipped workflow check = %#v, want args %q", shippedCheck, wantShippedArgs)
	}
	schemaCoverageCheck := checkByLabel(t, gotChecks, "schema-description-coverage")
	if schemaCoverageCheck.label != "schema-description-coverage" ||
		!reflect.DeepEqual(schemaCoverageCheck.args, []string{"test", "-v", "-run", "^TestDescriptionCoverage$", "./api/schemas"}) {
		t.Fatalf("schema description coverage check = %#v", schemaCoverageCheck)
	}

	buildCheck := checkByLabel(t, gotChecks, "build-goobers")
	if got := filepath.ToSlash(strings.Join(buildCheck.args, " ")); !strings.Contains(got, "-o bin/goobers ./cmd/goobers") {
		t.Fatalf("goobers build args = %q", got)
	}
	if got := strings.Join(buildCheck.args, " "); !strings.Contains(got, versionPackage+".Version=v1.2.3") {
		t.Fatalf("goobers build args missing metadata: %q", got)
	}
	validateCheck := checkByLabel(t, gotChecks, "validate-configs")
	if got := filepath.ToSlash(strings.Join(validateCheck.args, " ")); got != "run ./test/configvalidate bin/goobers" {
		t.Fatalf("validate-configs args = %q", got)
	}
}

func TestFastChecksAreStrictMergeGateSubset(t *testing.T) {
	t.Parallel()
	mergeChecks := checks(
		[]string{"config-sync", "goobers", "operator"},
		toolchain{
			goCommand:       "go",
			gofmtCommand:    "gofmt",
			gitCommand:      "git",
			npmCommand:      "npm",
			golangciCommand: "golangci-lint",
		},
		buildMetadata{},
		"linux",
		"",
	)
	fast := fastChecks(mergeChecks)

	var got []string
	for _, current := range fast {
		got = append(got, current.label)
	}
	want := []string{
		"fmt-check",
		"no-phone-home",
		"vet",
		"build-config-sync",
		"build-goobers",
		"build-operator",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fast check order = %q, want %q", got, want)
	}
	if len(fast) >= len(mergeChecks) {
		t.Fatalf("fast tier has %d checks, merge tier has %d; want a strict subset", len(fast), len(mergeChecks))
	}
	if args := checkByLabel(t, fast, "build-goobers").args; slices.Contains(args, "embed_portal") {
		t.Fatalf("fast Goobers build requires generated Portal assets: %q", args)
	}

	mergeLabels := make(map[string]bool, len(mergeChecks))
	for _, current := range mergeChecks {
		mergeLabels[current.label] = true
	}
	for _, current := range fast {
		if !mergeLabels[current.label] {
			t.Errorf("fast check %q is absent from the merge tier", current.label)
		}
	}
}

func TestChecksRunTidyDiffWithConfiguredGo(t *testing.T) {
	t.Parallel()
	got := checks(
		nil,
		toolchain{goCommand: "custom-go", gofmtCommand: "gofmt", gitCommand: "git", npmCommand: "npm"},
		buildMetadata{},
		"linux",
		"",
	)

	for _, current := range got {
		if current.label != "tidy-check" {
			continue
		}
		if current.command != "custom-go" {
			t.Errorf("tidy-check command = %q, want custom-go", current.command)
		}
		if want := []string{"mod", "tidy", "-diff"}; !reflect.DeepEqual(current.args, want) {
			t.Errorf("tidy-check args = %q, want %q", current.args, want)
		}
		if len(current.env) != 0 {
			t.Errorf("tidy-check environment overrides = %q, want inherited module settings", current.env)
		}
		return
	}
	t.Fatal("checks do not include tidy-check")
}

func TestFullChecksRunEveryGateSeriallyWithElapsedReporting(t *testing.T) {
	t.Parallel()
	got := fullChecks("custom-make")
	want := []string{
		"ci",
		"test-integration-strict",
		"test-e2e",
		"test-conformance",
		"test-envtest",
		"cover-check",
		"sandbox-check",
		"linux-node-validation",
		"stress",
	}
	if len(got) != len(want) {
		t.Fatalf("full checks = %d, want %d", len(got), len(want))
	}
	for i, current := range got {
		if current.label != want[i] || current.command != "custom-make" ||
			!reflect.DeepEqual(current.args, []string{want[i]}) {
			t.Fatalf("full check %d = %#v, want custom-make %s", i, current, want[i])
		}
	}

	tick := int64(0)
	now := func() time.Time {
		current := time.Unix(tick, 0)
		tick++
		return current
	}
	exec := &fakeExecutor{}
	var stdout, stderr bytes.Buffer
	if err := executeChecksAt(exec, got, &stdout, &stderr, now); err != nil {
		t.Fatal(err)
	}
	if len(exec.calls) != len(want) {
		t.Fatalf("executed %d full checks, want %d", len(exec.calls), len(want))
	}
	for _, label := range want {
		if !strings.Contains(stdout.String(), "<== "+label+" (elapsed 1s)") {
			t.Errorf("stdout missing elapsed %s target:\n%s", label, &stdout)
		}
	}
}

func TestChecksUseWindowsExecutableSuffix(t *testing.T) {
	t.Parallel()
	got := checks(
		[]string{"goobers"},
		toolchain{goCommand: "go", gofmtCommand: "gofmt", gitCommand: "git", npmCommand: "npm"},
		buildMetadata{},
		"windows",
		"",
	)
	// Looked up by label, not position: a hardcoded index here previously
	// broke silently whenever a check was inserted earlier in the list.
	if args := filepath.ToSlash(strings.Join(checkByLabel(t, got, "build-goobers").args, " ")); !strings.Contains(args, "-o bin/goobers.exe") {
		t.Fatalf("Windows build args = %q", args)
	}
	if args := checkByLabel(t, got, "build-goobers").args; !slices.Contains(args, "embed_portal") {
		t.Fatalf("Windows build args = %q, want embed_portal tag", args)
	}
	if args := filepath.ToSlash(strings.Join(checkByLabel(t, got, "validate-configs").args, " ")); args != "run ./test/configvalidate bin/goobers.exe" {
		t.Fatalf("Windows validate-configs args = %q", args)
	}
	for _, current := range got {
		if current.label == "test" {
			if !slices.Contains(current.env, "CGO_ENABLED=1") {
				t.Fatalf("Windows test environment = %q, want CGO_ENABLED=1", current.env)
			}
			return
		}
	}
	t.Fatal("Windows checks do not include the test step")
}

func TestChecksPreparePortalWithoutGoobersCommand(t *testing.T) {
	t.Parallel()
	got := checks(
		[]string{"operator"},
		toolchain{goCommand: "go", gofmtCommand: "gofmt", gitCommand: "git", npmCommand: "npm"},
		buildMetadata{},
		"linux",
		"",
	)
	var labels []string
	for _, current := range got {
		labels = append(labels, current.label)
	}
	if strings.Join(labels, " ") != "fmt-check tidy-check no-phone-home stage-name-lint vet flake-policy design-doc-status markdown-links npm-registry go-toolchain build-operator portal-install portal-audit portal-playwright-install portal-build portal-embed-vet shipped-workflows schema-description-coverage test lint portal-test portal-deadcode portal-e2e portal-contract-generate portal-contract-diff portal-contract-typecheck portal-contract-test manifests-generate manifests-diff" {
		t.Fatalf("check order = %q", labels)
	}
}

func TestChecksInstallPinnedChromiumAndRunPortalE2E(t *testing.T) {
	t.Parallel()
	got := checks(
		[]string{"goobers"},
		toolchain{goCommand: "go", gofmtCommand: "gofmt", gitCommand: "git", npmCommand: "custom-npm"},
		buildMetadata{},
		"linux",
		"",
	)

	install := checkByLabel(t, got, "portal-playwright-install")
	if install.command != "custom-npm" {
		t.Errorf("portal-playwright-install command = %q, want custom-npm", install.command)
	}
	if want := []string{"--prefix", "portal", "exec", "--", "playwright", "install", "chromium"}; !reflect.DeepEqual(install.args, want) {
		t.Errorf("portal-playwright-install args = %q, want %q", install.args, want)
	}

	e2e := checkByLabel(t, got, "portal-e2e")
	if e2e.command != "custom-npm" {
		t.Errorf("portal-e2e command = %q, want custom-npm", e2e.command)
	}
	if want := []string{"--prefix", "portal", "run", "test:e2e"}; !reflect.DeepEqual(e2e.args, want) {
		t.Errorf("portal-e2e args = %q, want %q", e2e.args, want)
	}

	deadcode := checkByLabel(t, got, "portal-deadcode")
	if deadcode.command != "custom-npm" {
		t.Errorf("portal-deadcode command = %q, want custom-npm", deadcode.command)
	}
	if want := []string{"--prefix", "portal", "run", "deadcode"}; !reflect.DeepEqual(deadcode.args, want) {
		t.Errorf("portal-deadcode args = %q, want %q", deadcode.args, want)
	}

	if install.skip == nil {
		t.Fatal("portal-playwright-install has no skip hook; #3372 no-op on a read-only baked-browser PLAYWRIGHT_BROWSERS_PATH would regress")
	}
}

func TestPortalEmbedVetRunsAfterBuild(t *testing.T) {
	t.Parallel()
	got := checks(
		[]string{"goobers"},
		toolchain{goCommand: "go", gofmtCommand: "gofmt", gitCommand: "git", npmCommand: "npm"},
		buildMetadata{},
		"linux",
		"",
	)

	var vetIdx, buildIdx = -1, -1
	for i, current := range got {
		switch current.label {
		case "portal-embed-vet":
			vetIdx = i
		case "portal-build":
			buildIdx = i
		}
	}
	if vetIdx == -1 {
		t.Fatal("portal-embed-vet check is missing")
	}
	if vetIdx != buildIdx+1 {
		t.Fatalf("portal-embed-vet at %d, want immediately after portal-build at %d", vetIdx, buildIdx)
	}
	check := got[vetIdx]
	if check.command != "go" {
		t.Errorf("portal-embed-vet command = %q, want go", check.command)
	}
	if want := []string{"vet", "-tags", "embed_portal", "./internal/portalassets", "./cmd/goobers"}; !reflect.DeepEqual(check.args, want) {
		t.Errorf("portal-embed-vet args = %q, want %q", check.args, want)
	}
}

func TestGoobersBuildEmbedsPortalArtifact(t *testing.T) {
	t.Parallel()
	got := checks(
		[]string{"goobers"},
		toolchain{goCommand: "go", gofmtCommand: "gofmt", gitCommand: "git", npmCommand: "npm"},
		buildMetadata{},
		"linux",
		"",
	)
	build := checkByLabel(t, got, "build-goobers")
	if want := []string{"build", "-tags", "embed_portal", "-ldflags", "-X github.com/goobers/goobers/internal/version.Version= -X github.com/goobers/goobers/internal/version.Commit= -X github.com/goobers/goobers/internal/version.Date=", "-o", filepath.Join("bin", "goobers"), "./cmd/goobers"}; !reflect.DeepEqual(build.args, want) {
		t.Errorf("build-goobers args = %q, want %q", build.args, want)
	}
}

// TestManifestsDriftGuardRunsGitDiff locks the #2013 guard: the CRD manifest
// drift check runs immediately after its own regen step and is a
// `git diff --exit-code -- config/crd/bases`, so a regenerated manifest that
// differs from the committed one fails the gate — the same
// generate-then-diff shape as portal-contract-diff.
func TestManifestsDriftGuardRunsGitDiff(t *testing.T) {
	t.Parallel()
	got := checks(
		[]string{"goobers"},
		toolchain{goCommand: "go", gofmtCommand: "gofmt", gitCommand: "git", npmCommand: "npm"},
		buildMetadata{},
		"linux",
		"",
	)

	var diffIdx, generateIdx = -1, -1
	for i, current := range got {
		switch current.label {
		case "manifests-diff":
			diffIdx = i
		case "manifests-generate":
			generateIdx = i
		}
	}
	if generateIdx == -1 {
		t.Fatal("manifests-generate check is missing")
	}
	if diffIdx == -1 {
		t.Fatal("manifests-diff check is missing")
	}
	if diffIdx != generateIdx+1 {
		t.Fatalf("manifests-diff at %d, want immediately after manifests-generate at %d", diffIdx, generateIdx)
	}
	generate := got[generateIdx]
	if generate.command != "go" {
		t.Errorf("manifests-generate command = %q, want go", generate.command)
	}
	if want := []string{
		"run", "sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5",
		"crd:allowDangerousTypes=true", "paths=./api/v1alpha1/...", "output:crd:dir=config/crd/bases",
	}; !reflect.DeepEqual(generate.args, want) {
		t.Errorf("manifests-generate args = %q, want %q", generate.args, want)
	}
	guard := got[diffIdx]
	if guard.command != "git" {
		t.Errorf("manifests-diff command = %q, want git", guard.command)
	}
	if want := []string{"diff", "--exit-code", "--", "config/crd/bases"}; !reflect.DeepEqual(guard.args, want) {
		t.Errorf("manifests-diff args = %q, want %q", guard.args, want)
	}
}

func TestConfiguredToolchainUsesDefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	getenv := func(name string) string {
		if name == "GO" {
			return "custom-go"
		}
		if name == "NPM" {
			return "custom-npm"
		}
		return ""
	}
	got := configuredToolchain(getenv)
	want := toolchain{
		goCommand:       "custom-go",
		gofmtCommand:    "gofmt",
		gitCommand:      "git",
		npmCommand:      "custom-npm",
		golangciCommand: "golangci-lint",
	}
	if got != want {
		t.Fatalf("configuredToolchain() = %#v, want %#v", got, want)
	}
}

func TestCommandInvocationUsesStockWindowsShellForNPM(t *testing.T) {
	t.Parallel()
	current := check{
		command:      "npm",
		args:         []string{"--prefix", "portal", "test"},
		windowsBatch: true,
	}
	getenv := func(name string) string {
		if name == "ComSpec" {
			return `C:\Windows\System32\cmd.exe`
		}
		return ""
	}

	command, args := commandInvocation(current, "windows", getenv)
	if command != `C:\Windows\System32\cmd.exe` {
		t.Fatalf("command = %q", command)
	}
	wantArgs := []string{"/d", "/s", "/c", "npm", "--prefix", "portal", "test"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %q, want %q", args, wantArgs)
	}

	command, args = commandInvocation(current, "linux", getenv)
	if command != "npm" || !reflect.DeepEqual(args, current.args) {
		t.Fatalf("Unix invocation = %q %q", command, args)
	}
}

func TestCommandPackagesReturnsSortedDirectories(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	for _, name := range []string{"zeta", "alpha"} {
		if err := os.Mkdir(filepath.Join(directory, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("not a command"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := commandPackages(directory)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commandPackages() = %q, want %q", got, want)
	}
	if _, err := commandPackages(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("commandPackages() succeeded for a missing directory")
	}
}

func TestExecuteChecksRejectsUnformattedFiles(t *testing.T) {
	t.Parallel()
	exec := &fakeExecutor{
		outputs: map[string][]byte{"fmt-check": []byte("bad.go\n")},
	}
	var stdout, stderr bytes.Buffer
	err := executeChecks(exec, []check{
		{label: "fmt-check", expectEmpty: true},
		{label: "vet"},
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "expected no output") {
		t.Fatalf("executeChecks() error = %v", err)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executed %d checks, want 1", len(exec.calls))
	}
	if !strings.Contains(stdout.String(), "bad.go") {
		t.Fatalf("stdout missing unformatted file:\n%s", &stdout)
	}
}

func TestExecuteChecksRunsAllSuccessfulChecks(t *testing.T) {
	t.Parallel()
	exec := &fakeExecutor{}
	var stdout, stderr bytes.Buffer
	if err := executeChecks(exec, []check{
		{label: "fmt-check", expectEmpty: true},
		{label: "vet"},
	}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("executed %d checks, want 2", len(exec.calls))
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestChecksWrapUnitTestWhenTimingOutputIsConfigured(t *testing.T) {
	t.Parallel()
	got := checks(
		nil,
		toolchain{goCommand: "go", npmCommand: "npm", gitCommand: "git"},
		buildMetadata{},
		"linux",
		"test-timings/unit-Linux.json",
	)
	for _, current := range got {
		if current.label != "test" {
			continue
		}
		want := "run ./test/hermetic --go-command go --timing-job unit --timing-output test-timings/unit-Linux.json -- -race -timeout 30m -count=1 -covermode=atomic -coverprofile=coverage.out ./..."
		if args := strings.Join(current.args, " "); args != want {
			t.Fatalf("timed test args = %q, want %q", args, want)
		}
		return
	}
	t.Fatal("checks do not include the test step")
}

func TestExecuteChecksPrintsElapsedPerTarget(t *testing.T) {
	t.Parallel()
	times := []time.Time{
		time.Unix(0, 0),
		time.Unix(1, 250_000_000),
		time.Unix(2, 0),
		time.Unix(4, 500_000_000),
	}
	next := 0
	now := func() time.Time {
		value := times[next]
		next++
		return value
	}
	var stdout, stderr bytes.Buffer
	if err := executeChecksAt(
		&fakeExecutor{},
		[]check{{label: "fmt-check"}, {label: "vet"}},
		&stdout,
		&stderr,
		now,
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "<== fmt-check (elapsed 1.25s)") ||
		!strings.Contains(stdout.String(), "<== vet (elapsed 2.5s)") {
		t.Fatalf("stdout missing elapsed targets:\n%s", &stdout)
	}
}

func TestExecuteChecksStopsAtCommandFailure(t *testing.T) {
	t.Parallel()
	exec := &fakeExecutor{
		outputs:      map[string][]byte{"vet": []byte("vet failed")},
		failCommands: map[string]bool{"vet": true},
	}
	var stdout, stderr bytes.Buffer
	err := executeChecks(exec, []check{
		{label: "fmt-check"},
		{label: "vet"},
		{label: "build"},
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "vet") {
		t.Fatalf("executeChecks() error = %v", err)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("executed %d checks, want 2", len(exec.calls))
	}
	if stderr.String() != "vet failed\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestExecuteChecksRetriesTransientlyFailingCheck(t *testing.T) {
	t.Parallel()
	exec := &fakeExecutor{transientFailures: map[string]int{"portal-audit": 2}}
	var stdout, stderr bytes.Buffer
	if err := executeChecks(exec, []check{
		{label: "portal-audit", retries: 2},
	}, &stdout, &stderr); err != nil {
		t.Fatalf("executeChecks() error = %v", err)
	}
	if len(exec.calls) != 3 {
		t.Fatalf("executed %d attempts, want 3", len(exec.calls))
	}
	if !strings.Contains(stdout.String(), "portal-audit: attempt 1/3 failed") ||
		!strings.Contains(stdout.String(), "portal-audit: attempt 2/3 failed") {
		t.Fatalf("stdout missing retry notices:\n%s", &stdout)
	}
}

func TestExecuteChecksFailsAfterExhaustingRetries(t *testing.T) {
	t.Parallel()
	exec := &fakeExecutor{
		outputs:      map[string][]byte{"portal-audit": []byte("1 high severity vulnerability")},
		failCommands: map[string]bool{"portal-audit": true},
	}
	var stdout, stderr bytes.Buffer
	err := executeChecks(exec, []check{
		{label: "portal-audit", retries: 2},
		{label: "portal-build"},
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "portal-audit") {
		t.Fatalf("executeChecks() error = %v", err)
	}
	if len(exec.calls) != 3 {
		t.Fatalf("executed %d attempts, want 3", len(exec.calls))
	}
	if stderr.String() != "1 high severity vulnerability\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestPortalAuditRetriesTheRegistryAdvisoryEndpoint(t *testing.T) {
	t.Parallel()
	for _, current := range checks(nil, toolchain{goCommand: "go", npmCommand: "npm", gitCommand: "git"}, buildMetadata{}, "linux", "") {
		if current.label != "portal-audit" {
			continue
		}
		if current.retries != 2 {
			t.Fatalf("portal-audit retries = %d, want 2", current.retries)
		}
		if current.retryDelay <= 0 {
			t.Fatalf("portal-audit retryDelay = %s, want a positive pause between attempts", current.retryDelay)
		}
		return
	}
	t.Fatal("checks do not include portal-audit")
}

func TestResolveBuildMetadataUsesOverridesAndFallbacks(t *testing.T) {
	t.Parallel()
	now := func() time.Time {
		return time.Date(2026, 7, 20, 19, 30, 0, 0, time.FixedZone("offset", -7*60*60))
	}
	getenv := func(name string) string {
		if name == "VERSION" {
			return "custom"
		}
		return ""
	}
	exec := &fakeExecutor{
		outputs: map[string][]byte{
			"git rev-parse --short HEAD": []byte("abc1234\n"),
		},
	}

	got := resolveBuildMetadata(exec, toolchain{gitCommand: "git"}, now, getenv)
	want := buildMetadata{
		version: "custom",
		commit:  "abc1234",
		date:    "2026-07-21T02:30:00Z",
	}
	if got != want {
		t.Fatalf("resolveBuildMetadata() = %#v, want %#v", got, want)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("git calls = %d, want 1", len(exec.calls))
	}

	exec = &fakeExecutor{
		failCommands: map[string]bool{
			"git describe --tags --always --dirty": true,
			"git rev-parse --short HEAD":           true,
		},
	}
	got = resolveBuildMetadata(exec, toolchain{gitCommand: "git"}, now, func(string) string { return "" })
	if got.version != "dev" || got.commit != "none" {
		t.Fatalf("fallback metadata = %#v", got)
	}
}

func TestMergeEnvironmentReplacesVariables(t *testing.T) {
	t.Parallel()
	base := []string{"PATH=/bin", "keep=yes", "Mixed=old"}
	overrides := []string{"PATH=/tools", "MIXED=new"}

	got := mergeEnvironment(base, overrides, true)
	want := []string{"keep=yes", "PATH=/tools", "MIXED=new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnvironment() = %q, want %q", got, want)
	}
}

func TestProcessExecutorCapturesAndStreamsCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exec := processExecutor{stdout: &stdout, stderr: &stderr}
	helperArgs := []string{"-test.run=TestProcessExecutorHelper", "--"}

	output, err := exec.run(check{
		command: os.Args[0],
		args:    helperArgs,
		env:     []string{"GO_WANT_CI_HELPER_PROCESS=1", "CI_HELPER_VALUE=captured"},
		capture: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "captured" || stderr.Len() != 0 {
		t.Fatalf("captured output = %q", output)
	}

	output, err = exec.run(check{
		command:     os.Args[0],
		args:        helperArgs,
		env:         []string{"GO_WANT_CI_HELPER_PROCESS=1", "CI_HELPER_VALUE=formatted"},
		capture:     true,
		expectEmpty: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "formatted" || stderr.String() != "stderr" {
		t.Fatalf("stdout-only capture = %q, stderr = %q", output, stderr.String())
	}
	stderr.Reset()

	_, err = exec.run(check{
		command: os.Args[0],
		args:    helperArgs,
		env:     []string{"GO_WANT_CI_HELPER_PROCESS=1", "CI_HELPER_VALUE=streamed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "streamed" || stderr.String() != "stderr" {
		t.Fatalf("streamed output = %q, %q", stdout.String(), stderr.String())
	}

	output, err = exec.run(check{
		command: os.Args[0],
		args:    helperArgs,
		env:     []string{"GO_WANT_CI_HELPER_PROCESS=1", "CI_HELPER_VALUE=fail"},
		capture: true,
	})
	if err == nil || string(output) != "fail" {
		t.Fatalf("failed command output = %q, error = %v", output, err)
	}
}

func TestProcessExecutorHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CI_HELPER_PROCESS") != "1" {
		return
	}
	value := os.Getenv("CI_HELPER_VALUE")
	_, _ = fmt.Fprint(os.Stdout, value)
	_, _ = fmt.Fprint(os.Stderr, "stderr")
	if value == "fail" {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestRunRejectsArguments(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"test"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func mergeGateChecks() []check {
	tools := toolchain{
		goCommand:       "go",
		gofmtCommand:    "gofmt",
		gitCommand:      "git",
		npmCommand:      "npm",
		golangciCommand: "golangci-lint",
	}
	return checks([]string{"config-sync", "goobers", "operator"}, tools, buildMetadata{}, "linux", "")
}

// TestEveryMergeCheckHasAGroup guarantees the parallel CI jobs collectively run
// every merge-gate check: each check names one of the four known groups, and no
// check is orphaned into a group no job runs.
func TestEveryMergeCheckHasAGroup(t *testing.T) {
	t.Parallel()
	known := map[string]bool{groupChecks: true, groupLint: true, groupUnit: true, groupShipped: true}
	seen := map[string]bool{}
	for _, current := range mergeGateChecks() {
		if !known[current.group] {
			t.Errorf("check %q has group %q, not one of the CI job groups", current.label, current.group)
		}
		seen[current.group] = true
	}
	for group := range known {
		if !seen[group] {
			t.Errorf("no merge-gate check belongs to group %q; the %q job would run nothing", group, group)
		}
	}
}

// TestGroupPartitionCoversMergeGate proves that concatenating the four groups
// reproduces exactly the full merge-gate check set — no check is dropped or
// double-counted when the monolith fans out across runners.
func TestGroupPartitionCoversMergeGate(t *testing.T) {
	t.Parallel()
	all := mergeGateChecks()
	var reassembled []string
	for _, group := range []string{groupChecks, groupLint, groupUnit, groupShipped} {
		for _, current := range groupChecksOnly(all, group) {
			reassembled = append(reassembled, current.label)
		}
	}
	wantCount := len(all)
	if len(reassembled) != wantCount {
		t.Fatalf("groups cover %d checks, merge gate has %d", len(reassembled), wantCount)
	}
	slices.Sort(reassembled)
	var want []string
	for _, current := range all {
		want = append(want, current.label)
	}
	slices.Sort(want)
	if !reflect.DeepEqual(reassembled, want) {
		t.Fatalf("group union = %q, want %q", reassembled, want)
	}
}

func TestGroupChecksOnlyIsolatesHeavyweights(t *testing.T) {
	t.Parallel()
	all := mergeGateChecks()
	for _, tc := range []struct {
		group string
		want  []string
	}{
		{groupLint, []string{"lint"}},
		{groupUnit, []string{"schema-description-coverage", "test"}},
		{groupShipped, []string{"shipped-workflows"}},
	} {
		var got []string
		for _, current := range groupChecksOnly(all, tc.group) {
			got = append(got, current.label)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("group %q = %q, want %q", tc.group, got, tc.want)
		}
	}
}

func labelArgs(checks []check, label string) []string {
	for _, current := range checks {
		if current.label == label {
			return current.args
		}
	}
	return nil
}

func TestApplyRuntimeTogglesDropsRaceWhenDisabled(t *testing.T) {
	t.Parallel()
	all := mergeGateChecks()
	env := func(name string) string {
		if name == "GOOBERS_CI_RACE" {
			return "0"
		}
		return ""
	}
	unit := applyRuntimeToggles(groupChecksOnly(all, groupUnit), env)
	if slices.Contains(labelArgs(unit, "test"), "-race") {
		t.Errorf("unit test retained -race with GOOBERS_CI_RACE=0: %q", labelArgs(unit, "test"))
	}
	shipped := applyRuntimeToggles(groupChecksOnly(all, groupShipped), env)
	if slices.Contains(labelArgs(shipped, "shipped-workflows"), "-race") {
		t.Errorf("shipped suite retained -race with GOOBERS_CI_RACE=0: %q", labelArgs(shipped, "shipped-workflows"))
	}
}

func TestApplyRuntimeTogglesKeepsRaceByDefault(t *testing.T) {
	t.Parallel()
	all := mergeGateChecks()
	unit := applyRuntimeToggles(groupChecksOnly(all, groupUnit), func(string) string { return "" })
	if !slices.Contains(labelArgs(unit, "test"), "-race") {
		t.Errorf("unit test dropped -race by default: %q", labelArgs(unit, "test"))
	}
}

func TestApplyRuntimeTogglesShardsUnitSuite(t *testing.T) {
	t.Parallel()
	all := mergeGateChecks()
	env := func(name string) string {
		if name == "GOOBERS_CI_SHARD" {
			return "2/3"
		}
		return ""
	}
	unit := applyRuntimeToggles(groupChecksOnly(all, groupUnit), env)
	args := labelArgs(unit, "test")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--shard 2/3 --") {
		t.Errorf("unit args missing shard flag before separator: %q", joined)
	}
	if slices.Contains(args, "-coverprofile=coverage.out") || slices.Contains(args, "-covermode=atomic") {
		t.Errorf("sharded unit args retained coverage flags: %q", joined)
	}
	if !slices.Contains(args, "-race") {
		t.Errorf("sharded unit args dropped -race unexpectedly: %q", joined)
	}
	if !slices.Contains(args, "./...") {
		t.Errorf("sharded unit args lost the package spec (hermetic expands ./...): %q", joined)
	}
	// -count=1 must SURVIVE sharding even though the coverage flags do not. It
	// is what makes deleting the dedicated `conformance` job a no-op: that job's
	// only behavioural difference from the shards was running uncached, and the
	// 32 TestConformance* functions it selected already execute here, unfiltered.
	if !slices.Contains(args, "-count=1") {
		t.Errorf("sharded unit args dropped -count=1; the shards must run uncached now that the conformance job is gone: %q", joined)
	}
}

func TestApplyRuntimeTogglesOverridesTestTimeout(t *testing.T) {
	t.Parallel()
	env := func(name string) string {
		if name == "GOOBERS_CI_TEST_TIMEOUT" {
			return "60m"
		}
		return ""
	}
	unit := applyRuntimeToggles(groupChecksOnly(mergeGateChecks(), groupUnit), env)
	if args := labelArgs(unit, "test"); !slices.Contains(args, "60m") || slices.Contains(args, "30m") {
		t.Errorf("unit timeout was not overridden: %q", args)
	}
	shipped := applyRuntimeToggles(groupChecksOnly(mergeGateChecks(), groupShipped), env)
	if args := labelArgs(shipped, "shipped-workflows"); !slices.Contains(args, "60m") || slices.Contains(args, "20m") {
		t.Errorf("shipped timeout was not overridden: %q", args)
	}
}

func TestApplyRuntimeTogglesCrossLintsViaSubprocessEnv(t *testing.T) {
	t.Parallel()
	all := mergeGateChecks()
	env := func(name string) string {
		if name == "GOOBERS_LINT_GOOS" {
			return "darwin"
		}
		return ""
	}
	lint := applyRuntimeToggles(groupChecksOnly(all, groupLint), env)
	var lintCheck check
	for _, current := range lint {
		if current.label == "lint" {
			lintCheck = current
		}
	}
	// GOOS goes into the golangci-lint subprocess env, not the launcher's args.
	if !slices.Contains(lintCheck.env, "GOOS=darwin") {
		t.Errorf("lint check env = %q, want it to contain GOOS=darwin", lintCheck.env)
	}
	if slices.Contains(lintCheck.args, "GOOS=darwin") {
		t.Errorf("GOOS must not leak into lint args (would cross-build the launcher): %q", lintCheck.args)
	}
}

func TestRunRejectsUnknownGroup(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"group", "nope"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown check group") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// checkByLabel returns the single check with the given label, failing if it is
// absent or ambiguous. Positional indexing into checks() is fragile: inserting
// a check shifts every later index, and the resulting assertion failure points
// at the wrong check rather than at the insertion.
func checkByLabel(t *testing.T, all []check, label string) check {
	t.Helper()
	var found []check
	for _, current := range all {
		if current.label == label {
			found = append(found, current)
		}
	}
	if len(found) != 1 {
		t.Fatalf("checks with label %q = %d, want exactly 1", label, len(found))
	}
	return found[0]
}

// TestLintIsSafeAcrossConcurrentWorktrees proves concurrent local CI runs queue
// on golangci-lint's process lock and never share its path-sensitive cache.
func TestLintIsSafeAcrossConcurrentWorktrees(t *testing.T) {
	t.Parallel()
	lint := checkByLabel(t, mergeGateChecks(), "lint")

	if !slices.Contains(lint.args, "--allow-serial-runners") {
		t.Fatalf("lint args = %q, want --allow-serial-runners to queue concurrent local CI runs", lint.args)
	}

	var cache string
	for _, variable := range lint.env {
		if name, value, found := strings.Cut(variable, "="); found && name == "GOLANGCI_LINT_CACHE" {
			cache = value
		}
	}
	if cache == "" {
		t.Fatal("lint check does not pin GOLANGCI_LINT_CACHE; concurrent worktrees would share one cache and one lock")
	}
	if !filepath.IsAbs(cache) {
		t.Fatalf("GOLANGCI_LINT_CACHE = %q, want an absolute path", cache)
	}

	workdir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if strings.HasPrefix(cache, workdir+string(os.PathSeparator)) {
		t.Fatalf("GOLANGCI_LINT_CACHE = %q lives inside the worktree; the checkout must stay clean", cache)
	}
}

// TestGolangciCacheIsDistinctPerWorkingDirectory proves the key is the working
// directory, so two sibling run worktrees can never collide, while repeated runs
// in one checkout keep a warm cache.
// Not parallel: t.Chdir is incompatible with a parallel test, and the working
// directory is exactly what this asserts on.
func TestGolangciCacheIsDistinctPerWorkingDirectory(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()

	cacheFor := func(dir string) string {
		t.Helper()
		t.Chdir(dir)
		env := golangciCacheEnvironment()
		if len(env) != 1 {
			t.Fatalf("golangciCacheEnvironment() = %v, want exactly one variable", env)
		}
		return env[0]
	}

	firstCache := cacheFor(first)
	secondCache := cacheFor(second)
	if firstCache == secondCache {
		t.Fatal("two working directories resolved to one golangci-lint cache; sibling worktrees would share a lock and a path space")
	}
	if repeat := cacheFor(first); repeat != firstCache {
		t.Fatalf("one working directory resolved to two golangci-lint caches (%q then %q); the cache would never stay warm", firstCache, repeat)
	}
}

// TestPinnedChromiumRevisionParsesBrowsersManifest locks the parsing of the
// exact file Playwright's own installer reads (playwright-core's
// browsers.json) to decide the chromium build a given package version
// wants. #3372's skip decision hinges on getting this revision right.
func TestPinnedChromiumRevisionParsesBrowsersManifest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		json    string
		want    string
		wantErr bool
	}{
		{
			name: "real-shape manifest",
			json: `{"browsers":[
				{"name":"chromium","revision":"1234","installByDefault":true},
				{"name":"firefox","revision":"1538","installByDefault":true}
			]}`,
			want: "1234",
		},
		{
			name:    "no chromium entry",
			json:    `{"browsers":[{"name":"firefox","revision":"1538"}]}`,
			wantErr: true,
		},
		{
			name:    "empty chromium revision",
			json:    `{"browsers":[{"name":"chromium","revision":""}]}`,
			wantErr: true,
		},
		{
			name:    "malformed json",
			json:    `not json`,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := pinnedChromiumRevision([]byte(tc.json))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("pinnedChromiumRevision(%q) = %q, nil; want error", tc.json, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("pinnedChromiumRevision(%q) error = %v", tc.json, err)
			}
			if got != tc.want {
				t.Fatalf("pinnedChromiumRevision(%q) = %q, want %q", tc.json, got, tc.want)
			}
		})
	}
}

// TestResolvePortalPlaywrightSkip locks the #3372 decision matrix:
// portal-playwright-install only no-ops when PLAYWRIGHT_BROWSERS_PATH is set
// AND it already holds a completed install of the exact chromium revision
// portal's installed playwright-core package expects. Every other case
// (env unset, manifest missing/unparsable, revision absent or incomplete)
// must report "do not skip" so the installer still runs — and, on a wrong
// bake, still fails loudly instead of silently passing.
//
// Not parallel: t.Chdir and t.Setenv are incompatible with a parallel test,
// and PLAYWRIGHT_BROWSERS_PATH plus the working directory are exactly what
// this asserts on.
func TestResolvePortalPlaywrightSkip(t *testing.T) {
	writeManifest := func(t *testing.T, root, revision string) {
		t.Helper()
		manifestDir := filepath.Join(root, "portal", "node_modules", "playwright-core")
		if err := os.MkdirAll(manifestDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		manifest := `{"browsers":[{"name":"chromium","revision":"` + revision + `","installByDefault":true}]}`
		if err := os.WriteFile(filepath.Join(manifestDir, "browsers.json"), []byte(manifest), 0o644); err != nil {
			t.Fatalf("WriteFile browsers.json: %v", err)
		}
	}

	t.Run("browsers path unset", func(t *testing.T) {
		root := t.TempDir()
		t.Chdir(root)
		t.Setenv(playwrightBrowsersPathEnv, "")

		skip, reason := resolvePortalPlaywrightSkip()
		if skip {
			t.Fatalf("resolvePortalPlaywrightSkip() = skip, %q; want do-not-skip", reason)
		}
		if !strings.Contains(reason, playwrightBrowsersPathEnv) {
			t.Fatalf("reason = %q, want it to name %s", reason, playwrightBrowsersPathEnv)
		}
	})

	t.Run("manifest missing", func(t *testing.T) {
		root := t.TempDir()
		t.Chdir(root)
		t.Setenv(playwrightBrowsersPathEnv, t.TempDir())

		skip, reason := resolvePortalPlaywrightSkip()
		if skip {
			t.Fatalf("resolvePortalPlaywrightSkip() = skip, %q; want do-not-skip (no node_modules yet)", reason)
		}
		if !strings.Contains(reason, "browsers.json") {
			t.Fatalf("reason = %q, want it to mention browsers.json", reason)
		}
	})

	t.Run("revision absent from browsers path", func(t *testing.T) {
		root := t.TempDir()
		writeManifest(t, root, "1234")
		t.Chdir(root)
		t.Setenv(playwrightBrowsersPathEnv, t.TempDir())

		skip, reason := resolvePortalPlaywrightSkip()
		if skip {
			t.Fatalf("resolvePortalPlaywrightSkip() = skip, %q; want do-not-skip (revision not installed)", reason)
		}
		if !strings.Contains(reason, "1234") {
			t.Fatalf("reason = %q, want it to name the missing revision 1234", reason)
		}
	})

	t.Run("wrong revision present", func(t *testing.T) {
		root := t.TempDir()
		writeManifest(t, root, "1234")
		t.Chdir(root)
		browsersDir := t.TempDir()
		wrongDir := filepath.Join(browsersDir, "chromium-9999")
		if err := os.MkdirAll(wrongDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(wrongDir, "INSTALLATION_COMPLETE"), nil, 0o644); err != nil {
			t.Fatalf("WriteFile marker: %v", err)
		}
		t.Setenv(playwrightBrowsersPathEnv, browsersDir)

		skip, reason := resolvePortalPlaywrightSkip()
		if skip {
			t.Fatalf("resolvePortalPlaywrightSkip() = skip, %q; want do-not-skip (wrong bake must still fail loudly)", reason)
		}
		if !strings.Contains(reason, "1234") {
			t.Fatalf("reason = %q, want it to name the expected revision 1234", reason)
		}
	})

	t.Run("revision present but incomplete", func(t *testing.T) {
		root := t.TempDir()
		writeManifest(t, root, "1234")
		t.Chdir(root)
		browsersDir := t.TempDir()
		// Directory exists (e.g. a download that was interrupted) but no
		// INSTALLATION_COMPLETE marker: Playwright itself would not treat
		// this as a finished install, so neither should we.
		if err := os.MkdirAll(filepath.Join(browsersDir, "chromium-1234"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		t.Setenv(playwrightBrowsersPathEnv, browsersDir)

		skip, reason := resolvePortalPlaywrightSkip()
		if skip {
			t.Fatalf("resolvePortalPlaywrightSkip() = skip, %q; want do-not-skip (no completion marker)", reason)
		}
		if !strings.Contains(reason, "1234") {
			t.Fatalf("reason = %q, want it to name revision 1234", reason)
		}
	})

	t.Run("matching revision preinstalled", func(t *testing.T) {
		root := t.TempDir()
		writeManifest(t, root, "1234")
		t.Chdir(root)
		browsersDir := t.TempDir()
		chromiumDir := filepath.Join(browsersDir, "chromium-1234")
		if err := os.MkdirAll(chromiumDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(chromiumDir, "INSTALLATION_COMPLETE"), nil, 0o644); err != nil {
			t.Fatalf("WriteFile marker: %v", err)
		}
		t.Setenv(playwrightBrowsersPathEnv, browsersDir)

		skip, reason := resolvePortalPlaywrightSkip()
		if !skip {
			t.Fatalf("resolvePortalPlaywrightSkip() = do-not-skip, %q; want skip", reason)
		}
		if !strings.Contains(reason, chromiumDir) {
			t.Fatalf("reason = %q, want it to name the resolved path %s", reason, chromiumDir)
		}
	})
}

// TestExecuteChecksSkipsWithoutRunningTheCommand locks the #3372 wiring at
// the executeChecksAt level: when a check's skip hook reports skip=true, the
// executor never runs the underlying command and the returned reason is
// logged in its place, but the check still counts as a pass (the gate keeps
// going) and still reports an elapsed line like every other check.
func TestExecuteChecksSkipsWithoutRunningTheCommand(t *testing.T) {
	t.Parallel()
	exec := &fakeExecutor{}
	var stdout, stderr bytes.Buffer
	err := executeChecks(exec, []check{
		{
			label:   "portal-playwright-install",
			command: "npm",
			args:    []string{"--prefix", "portal", "exec", "--", "playwright", "install", "chromium"},
			skip: func() (bool, string) {
				return true, "browsers preinstalled at /opt/ms-playwright/chromium-1234, skipping"
			},
		},
		{label: "portal-build"},
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("executeChecks() error = %v", err)
	}
	if len(exec.calls) != 1 || exec.calls[0].label != "portal-build" {
		t.Fatalf("exec.calls = %v, want only portal-build (portal-playwright-install skipped)", exec.calls)
	}
	if !strings.Contains(stdout.String(), "portal-playwright-install: browsers preinstalled at /opt/ms-playwright/chromium-1234, skipping") {
		t.Fatalf("stdout missing skip reason:\n%s", &stdout)
	}
	if !strings.Contains(stdout.String(), "<== portal-playwright-install (elapsed") {
		t.Fatalf("stdout missing elapsed line for skipped check:\n%s", &stdout)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty (skip is not a failure)", stderr.String())
	}
}

// TestExecuteChecksRunsCommandWhenSkipHookDeclines locks the other half of
// #3372: when the skip hook reports skip=false (env unset, wrong revision,
// ...), the check runs exactly as if it had no hook at all.
func TestExecuteChecksRunsCommandWhenSkipHookDeclines(t *testing.T) {
	t.Parallel()
	exec := &fakeExecutor{}
	var stdout, stderr bytes.Buffer
	err := executeChecks(exec, []check{
		{
			label:   "portal-playwright-install",
			command: "npm",
			args:    []string{"--prefix", "portal", "exec", "--", "playwright", "install", "chromium"},
			skip: func() (bool, string) {
				return false, "PLAYWRIGHT_BROWSERS_PATH not set"
			},
		},
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("executeChecks() error = %v", err)
	}
	if len(exec.calls) != 1 || exec.calls[0].label != "portal-playwright-install" {
		t.Fatalf("exec.calls = %v, want portal-playwright-install to run", exec.calls)
	}
	if strings.Contains(stdout.String(), "PLAYWRIGHT_BROWSERS_PATH not set") {
		t.Fatalf("stdout unexpectedly logged the decline reason (only the skip branch should log):\n%s", &stdout)
	}
}
