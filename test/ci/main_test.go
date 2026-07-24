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

	gotChecks := checks([]string{"config-sync", "goobers", "scheduler"}, tools, metadata, "linux", "")
	var got []string
	for _, current := range gotChecks {
		got = append(got, current.label)
	}
	want := []string{
		"fmt-check",
		"tidy-check",
		"vet",
		"build-config-sync",
		"portal-install",
		"portal-build",
		"portal-dist-diff",
		"build-goobers",
		"validate-configs",
		"build-scheduler",
		"shipped-workflows",
		"test",
		"lint",
		"portal-test",
		"portal-contract-generate",
		"portal-contract-diff",
		"portal-contract-typecheck",
		"portal-contract-test",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("check order = %q, want %q", got, want)
	}

	testCheck := gotChecks[11]
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
		"-race", "-timeout", "20m", "-covermode=atomic", "-coverprofile=coverage.out", "./...",
	}
	if !reflect.DeepEqual(testCheck.args, wantTestArgs) {
		t.Fatalf("test arguments = %q, want %q", testCheck.args, wantTestArgs)
	}
	shippedCheck := gotChecks[10]
	if shippedCheck.label != "shipped-workflows" ||
		!reflect.DeepEqual(shippedCheck.args, []string{"test", "-race", "-timeout", "20m", "-count=1", "./test/shippedworkflows"}) {
		t.Fatalf("shipped workflow check = %#v", shippedCheck)
	}

	buildCheck := gotChecks[7]
	if got := filepath.ToSlash(strings.Join(buildCheck.args, " ")); !strings.Contains(got, "-o bin/goobers ./cmd/goobers") {
		t.Fatalf("goobers build args = %q", got)
	}
	if got := strings.Join(buildCheck.args, " "); !strings.Contains(got, versionPackage+".Version=v1.2.3") {
		t.Fatalf("goobers build args missing metadata: %q", got)
	}
	validateCheck := gotChecks[8]
	if got := filepath.ToSlash(strings.Join(validateCheck.args, " ")); got != "run ./test/configvalidate bin/goobers" {
		t.Fatalf("validate-configs args = %q", got)
	}
}

func TestFastChecksAreStrictMergeGateSubset(t *testing.T) {
	t.Parallel()
	mergeChecks := checks(
		[]string{"config-sync", "goobers", "scheduler"},
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
		"vet",
		"build-config-sync",
		"build-goobers",
		"build-scheduler",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fast check order = %q, want %q", got, want)
	}
	if len(fast) >= len(mergeChecks) {
		t.Fatalf("fast tier has %d checks, merge tier has %d; want a strict subset", len(fast), len(mergeChecks))
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
	if args := filepath.ToSlash(strings.Join(got[6].args, " ")); !strings.Contains(args, "-o bin/goobers.exe") {
		t.Fatalf("Windows build args = %q", args)
	}
	if args := filepath.ToSlash(strings.Join(got[7].args, " ")); args != "run ./test/configvalidate bin/goobers.exe" {
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
		[]string{"scheduler"},
		toolchain{goCommand: "go", gofmtCommand: "gofmt", gitCommand: "git", npmCommand: "npm"},
		buildMetadata{},
		"linux",
		"",
	)
	var labels []string
	for _, current := range got {
		labels = append(labels, current.label)
	}
	if strings.Join(labels, " ") != "fmt-check tidy-check vet build-scheduler portal-install portal-build portal-dist-diff shipped-workflows test lint portal-test portal-contract-generate portal-contract-diff portal-contract-typecheck portal-contract-test" {
		t.Fatalf("check order = %q", labels)
	}
}

// TestPortalDistDriftGuardRunsGitDiff locks the #1110 guard: the portal-dist
// drift check runs immediately after portal-build and is a
// `git diff --exit-code -- cmd/goobers/portal-dist`, so a rebuilt bundle that
// differs from the committed //go:embed-ed one fails the gate.
func TestPortalDistDriftGuardRunsGitDiff(t *testing.T) {
	t.Parallel()
	got := checks(
		[]string{"goobers"},
		toolchain{goCommand: "go", gofmtCommand: "gofmt", gitCommand: "git", npmCommand: "npm"},
		buildMetadata{},
		"linux",
		"",
	)

	var diffIdx, buildIdx = -1, -1
	for i, current := range got {
		switch current.label {
		case "portal-dist-diff":
			diffIdx = i
		case "portal-build":
			buildIdx = i
		}
	}
	if diffIdx == -1 {
		t.Fatal("portal-dist-diff check is missing")
	}
	if diffIdx != buildIdx+1 {
		t.Fatalf("portal-dist-diff at %d, want immediately after portal-build at %d", diffIdx, buildIdx)
	}
	guard := got[diffIdx]
	if guard.command != "git" {
		t.Errorf("portal-dist-diff command = %q, want git", guard.command)
	}
	if want := []string{"diff", "--exit-code", "--", "cmd/goobers/portal-dist"}; !reflect.DeepEqual(guard.args, want) {
		t.Errorf("portal-dist-diff args = %q, want %q", guard.args, want)
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
	if err == nil || !strings.Contains(err.Error(), "not gofmt-clean") {
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
		want := "run ./test/hermetic --go-command go --timing-job unit --timing-output test-timings/unit-Linux.json -- -race -timeout 20m -covermode=atomic -coverprofile=coverage.out ./..."
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
