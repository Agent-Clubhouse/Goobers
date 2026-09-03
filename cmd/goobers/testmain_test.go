package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// suiteRunWaitTimeout is the generous idle ceiling every wait-mode `goobers run`/
// `signal` in this suite gets (via runTerminalWaitTimeout, set in TestMain). It
// bounds time WITHOUT journal progress, not total run time, so a nested demo run
// that merely crawls under heavy concurrent make-ci load still finishes, while a
// genuine wedge — which appends nothing at all — fails ~5x faster than the
// 10-minute local-ci stage limit, turning a silent queue-wedging hang into a
// loud, diagnosable test failure (#827 recurrence guard). Deliberately not tight
// (e.g. 5s) so a slow-but-advancing stage never false-fails between events.
const suiteRunWaitTimeout = 2 * time.Minute

// hermeticEphemeralListen is the address every daemon-lifecycle test binds in
// place of the fixed default, so the OS hands out a free port instead (#798).
const hermeticEphemeralListen = "127.0.0.1:0"

type testCopilotModelLister struct{}

func (testCopilotModelLister) ListModels(context.Context, []string, []string) ([]harness.CopilotModelInfo, error) {
	return []harness.CopilotModelInfo{
		{ID: "claude-fable-5", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-sonnet-5", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-sonnet-4.6", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "max"}},
		{ID: "claude-sonnet-4.5"},
		{ID: "claude-haiku-4.5"},
		{ID: "claude-opus-4.8-fast", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-opus-4.8", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-opus-4.7", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-opus-4.6", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "max"}},
		{ID: "claude-opus-4.5"},
		{ID: "gpt-5.6-sol", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "gpt-5.6-terra", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "gpt-5.6-luna", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "gpt-5.5", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh"}},
		{ID: "gpt-5.4", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh"}},
		{ID: "gpt-5.3-codex", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh"}},
		{ID: "gpt-5.4-mini", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh"}},
		{ID: "gpt-5-mini", SupportedReasoningEfforts: []string{"none", "low", "medium", "high"}},
		{ID: "gemini-3.1-pro-preview", SupportedReasoningEfforts: []string{"none", "low", "medium", "high"}},
		{ID: "gemini-3.5-flash", SupportedReasoningEfforts: []string{"none", "minimal", "low", "medium", "high"}},
		{ID: "kimi-k2.7-code"},
		{ID: "mai-code-1-flash-picker"},
	}, nil
}

// TestMain arms the whole-suite seams before running any cmd/goobers test:
//
//  1. It neutralizes harness preflight and model discovery. These tests drive
//     `goobers up`/`run` against configs that declare agentic stages, but CI has
//     no real, installed Copilot CLI, so the production preflight
//     (LookPath("copilot")) and authenticated models.list query would fail every
//     such test. The real preflight logic and model-resolution behavior are
//     exercised directly in their package tests.
//
//  2. It makes every daemon-starting test hermetic (#798). A scaffolded instance
//     defaults to instance.DefaultAPIListenAddress (127.0.0.1:8080), so any test
//     that started `goobers up` bound that fixed port — and deterministically
//     collided with the self-host daemon already holding it during a live
//     `go test -race ./cmd/goobers/` run, wedging the whole package. This
//     redirect rewrites ONLY the fixed default to an ephemeral loopback port; a
//     test that deliberately sets its own address (http_lifecycle_test.go's
//     free-port and occupied-port cases) is passed through untouched. It is the
//     path of least resistance — no per-test setup needed — and the structural
//     guard that no test can bind the non-ephemeral default (asserted directly
//     by TestDaemonTestsNeverBindDefaultPort).
//
//  3. It disables git fsync for every git subprocess these tests spawn (#811).
//     See disableGitFsyncForTests.
//
//  4. It disables git's automatic background housekeeping for every git
//     subprocess these tests spawn (#3172). See
//     disableGitAutoMaintenanceForTests.
func TestMain(m *testing.M) {
	// Most CLI tests initialize ordinary t.TempDir fixtures. Do not inherit a
	// GitHub-hosted runner classification into those fixtures; tests that
	// exercise the hosted policy set this marker explicitly with t.Setenv.
	if err := os.Unsetenv("RUNNER_ENVIRONMENT"); err != nil {
		fmt.Fprintf(os.Stderr, "clear RUNNER_ENVIRONMENT for test fixtures: %v\n", err)
		os.Exit(1)
	}
	if os.Getenv(docsDryRunMakeEnv) == "1" && isDocsDryRunMakeProcess() {
		os.Exit(runDocsDryRunMake())
	}
	if baseURL := os.Getenv("GOOBERS_TEST_GITHUB_API_URL"); baseURL != "" {
		newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
			return providers.NewGitHubProvider(token, append(opts, func(provider *providers.GitHubProvider) {
				provider.BaseURL = baseURL
			})...)
		}
	}
	copilotModelLister = testCopilotModelLister{}

	// Keep the daemon suite hermetic against the machine's own memory (#3960).
	// newDaemonScheduler wires the cgroup-aware admission gate by default, so
	// without this every test that expects a dispatch would depend on the
	// runner's cgroup staying below the high-water mark — and this repo's CI
	// runs on-pod inside exactly the 10Gi cgroup that gate exists to protect,
	// so those tests would fail precisely when the feature is working. Tests
	// that exercise the gate itself override this with t.Setenv.
	if _, set := os.LookupEnv(memoryHighWaterEnv); !set {
		if err := os.Setenv(memoryHighWaterEnv, "off"); err != nil {
			fmt.Fprintf(os.Stderr, "set %s: %v\n", memoryHighWaterEnv, err)
			os.Exit(1)
		}
	}

	// Deterministic stages substitute os.Executable for a bare "goobers"
	// command. Let subprocesses launched that way exercise the real CLI
	// dispatcher instead of handing stage arguments to testing's flag parser.
	if os.Getenv("GOOBERS_RUN_ID") != "" && len(os.Args) > 1 {
		switch os.Args[1] {
		case "validate", "backlog-query", "docs-churn", "push-branch", "check-fail-first", "open-pr", demoProviderCommand:
			os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
		}
	}

	preflightHarnesses = func(map[string]apiv1.GooberSpec, []apiv1.Workflow, []string, map[string][]string, func(context.Context) (string, error)) (harnessPreflightInfo, error) {
		return harnessPreflightInfo{}, nil
	}

	baseAPIListenAddress := apiListenAddress
	apiListenAddress = func(c *instance.Config) string {
		if addr := baseAPIListenAddress(c); addr != instance.DefaultAPIListenAddress {
			return addr
		}
		return hermeticEphemeralListen
	}

	disableGitFsyncForTests()
	disableGitAutoMaintenanceForTests()
	disableGitLineEndingConversionForTests()
	disableJournalFsyncForTests()
	runTerminalWaitTimeout = suiteRunWaitTimeout

	packageDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "package directory guard: resolve working directory: %v\n", err)
		os.Exit(1)
	}
	guard, err := newPackageDirGuard(packageDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "package directory guard: snapshot %s: %v\n", packageDir, err)
		os.Exit(1)
	}

	code := m.Run()
	if changes := guard.changes(); len(changes) > 0 {
		fmt.Fprintln(os.Stderr, packageDirGuardFailure(packageDir, changes))
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// installMakeExecutableFixture writes a copy of this test binary into dir as
// name (a "make"/"make.exe" fixture that TestMain re-dispatches to a fake
// `make` implementation, see installDocsDryRunMake/installPortalBuildMake).
// It COPIES the executable rather than hard-linking it: a hard link shares
// the same underlying file object as the source, and on Windows the OS holds
// a delete-lock on that file object for as long as ANY name for it is the
// running image of a process — including the "go test" process this helper's
// own caller is running inside of, not just the short-lived make subprocess.
// A hard-linked fixture therefore stayed locked for the whole remaining
// lifetime of the test binary, so t.TempDir()'s eventual RemoveAll always hit
// "Access is denied", long after the make subprocess itself had exited and
// been fully cmd.Wait()'d. A copy is an independent file whose own lock is
// released once ITS OWN process exits, decoupled from the parent test
// binary's lifetime.
func installMakeExecutableFixture(t *testing.T, dir, name string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable for %s fixture: %v", name, err)
	}
	src, err := os.Open(executable)
	if err != nil {
		t.Fatalf("open test executable for %s fixture: %v", name, err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("create %s fixture: %v", name, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		t.Fatalf("copy %s fixture: %v", name, err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close %s fixture: %v", name, err)
	}
}

// TestJournalFsyncDisabledForSuite is the #827 recurrence guard, the journal-side
// twin of TestGitFsyncDisabledForSuite (#811): it asserts the journal
// fsync-disable seam is actually in effect for this suite. If someone drops
// disableJournalFsyncForTests() from TestMain or JOURNAL_TEST_FSYNC_OFF from the
// Makefile's test target, this test goes red instead of the whole local-ci stage
// silently wedging under concurrent make-ci disk saturation again. It asserts the
// env only (the journal package reads it per call; internal/journal/fsync_test.go
// covers that the env actually short-circuits the fsync).
func TestJournalFsyncDisabledForSuite(t *testing.T) {
	if got := os.Getenv("GOOBERS_DISABLE_FSYNC"); got != "1" {
		t.Fatalf("GOOBERS_DISABLE_FSYNC = %q, want \"1\" — the #827 journal fsync-disable seam "+
			"(disableJournalFsyncForTests / Makefile JOURNAL_TEST_FSYNC_OFF) is not in effect; "+
			"concurrent make ci can wedge on a journal fsync again", got)
	}
}

// disableGitFsyncForTests makes every git subprocess this suite spawns — the
// throwaway fixtures (newDaemonFixtureRepo) AND the real runner's own worktree
// clones/commits reached through `goobers run`/`up` — skip fsync. These repos
// are ephemeral t.TempDir scratch with zero durability requirements.
//
// Why it matters (#811): fsync is the one git syscall that blocks in
// uninterruptible I/O sleep under disk contention. When the self-host instance
// runs several `local-ci` stages at once (each a full cold `make ci`), the
// combined compile + `-race` + concurrent-fixture write pressure made a single
// `git init/commit/clone`'s fsync wedge for the whole 10-minute stage limit —
// so cmd/goobers never finished and the overnight run opened 0 PRs. Skipping
// fsync keeps git writes in the page cache so they return promptly under load;
// nothing a test can observe changes (durability across a crash is irrelevant
// to a scratch repo the test deletes anyway). The Makefile's `test` target sets
// the same for the full `make ci` run; this covers a bare `go test ./cmd/goobers/`.
//
// The GIT_CONFIG_COUNT/KEY/VALUE trio (git 2.31+) layers config onto every
// child process without a file or touching the developer's global config, and
// appends to any count already present rather than clobbering it. Only
// core.fsync=none (git 2.36+) is used, not the deprecated core.fsyncObjectFiles
// — the latter makes git print a "deprecated" warning to stderr that pollutes
// the combined output callers like gatherPRContext parse.
func disableGitFsyncForTests() {
	appendGitConfigForTests("core.fsync", "none")
}

// disableGitAutoMaintenanceForTests stops every git subprocess this suite
// spawns from starting background housekeeping (#3172). A push into a fixture's
// bare `origin.git` makes receive-pack run `git gc --auto`, which by default
// detaches (gc.autoDetach) and outlives the test that triggered it. That orphan
// keeps creating and deleting files under `origin.git/objects/pack` while
// t.TempDir's RemoveAll is walking the very same directory, so cleanup failed
// with "unlinkat .../objects/pack: directory not empty" — a flake attributable
// to whichever test happened to own the temp dir (TestRebasePRResolvesLiteral-
// PathspecFilename in the reported stress run), not to any defect in it.
//
// internal/testgit already passes `-c gc.auto=0 -c maintenance.auto=0` for the
// fixture git commands it builds, but that covers only its own children; the
// runner code under test (worktree clones, rebase, push) shells out to git
// through production paths that inherit this process's environment instead.
// Layering the same settings via GIT_CONFIG_* closes that gap for both.
// gc.autoDetach is pinned as well so an explicitly requested gc stays in the
// foreground and finishes before its caller returns.
func disableGitAutoMaintenanceForTests() {
	appendGitConfigForTests("gc.auto", "0")
	appendGitConfigForTests("gc.autoDetach", "false")
	appendGitConfigForTests("maintenance.auto", "false")
}

func disableGitLineEndingConversionForTests() {
	appendGitConfigForTests("core.autocrlf", "false")
	appendGitConfigForTests("core.safecrlf", "false")
}

func appendGitConfigForTests(key, value string) {
	n := 0
	if existing := os.Getenv("GIT_CONFIG_COUNT"); existing != "" {
		if parsed, err := strconv.Atoi(existing); err == nil && parsed > 0 {
			n = parsed
		}
	}
	// os.Setenv only errors on a key containing '=' or NUL, which these literals
	// never do; TestGitFsyncDisabledForSuite verifies the config actually reached
	// a git child regardless, so an explicit discard matches the suite's
	// os.Setenv convention (see main_test.go) without a meaningless error path.
	_ = os.Setenv("GIT_CONFIG_KEY_"+strconv.Itoa(n), key)
	_ = os.Setenv("GIT_CONFIG_VALUE_"+strconv.Itoa(n), value)
	_ = os.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(n+1))
}

// disableJournalFsyncForTests makes the run/instance journal skip its own
// os.File.Sync() for this test process — the journal-side twin of
// disableGitFsyncForTests, and for the same #811 reason. These tests spin up
// real in-process `goobers run`/`up`/`signal` executions that fsync every
// journal event, checkpoint, and artifact write. Under the disk saturation of
// several concurrent `make ci` (each a cold `go test -race ./...`), one of those
// journal fsyncs wedges in uninterruptible I/O for the whole 10-minute stage, so
// `waitForRunTerminal` polls a run that never reaches a terminal phase and the
// stage times out having opened 0 PRs (the live hang that made runs unusable).
//
// Setting the env here (not in the Makefile) scopes the change precisely to the
// cmd/goobers test binary and any subprocess it re-execs (which inherit the
// env), leaving every other package's fsync-dependent tests — and all of
// production — untouched. The journal reads the env per call, so setting it in
// TestMain (after journal package init) takes effect. Scratch t.TempDir
// instances have no durability requirement, so nothing a test can observe
// changes.
func disableJournalFsyncForTests() {
	// os.Setenv only errors on a key containing '=' or NUL, neither of which
	// this literal has; the suite's convention (see disableGitFsyncForTests) is
	// to discard that impossible error explicitly.
	_ = os.Setenv("GOOBERS_DISABLE_FSYNC", "1")
}
