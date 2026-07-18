package main

import (
	"os"
	"strconv"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// suiteRunWaitTimeout is the generous ceiling every wait-mode `goobers run`/
// `signal` in this suite gets (via runTerminalWaitTimeout, set in TestMain). A
// nested demo run's trivial stages finish in seconds even under heavy concurrent
// make-ci load, so 2 minutes only ever fires on a genuine wedge — yet it fails
// ~5x faster than the 10-minute local-ci stage limit, turning a silent
// queue-wedging hang into a loud, diagnosable test failure (#827 recurrence
// guard). Deliberately not tight (e.g. 5s) so a merely-slow-under-load run never
// false-fails.
const suiteRunWaitTimeout = 2 * time.Minute

// hermeticEphemeralListen is the address every daemon-lifecycle test binds in
// place of the fixed default, so the OS hands out a free port instead (#798).
const hermeticEphemeralListen = "127.0.0.1:0"

// TestMain arms two whole-suite seams before running any cmd/goobers test:
//
//  1. It neutralizes the harness-preflight seam. These tests drive
//     `goobers up`/`run` against configs that declare agentic stages, but CI has
//     no real, installed Copilot CLI, so the production preflight
//     (LookPath("copilot")) would fail every such test. The real preflight logic
//     is exercised directly in preflight_test.go via preflightAgenticHarnesses,
//     so no coverage is lost by stubbing the wiring seam here.
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
func TestMain(m *testing.M) {
	preflightHarnesses = func(map[string]apiv1.GooberSpec, []apiv1.Workflow) error { return nil }

	baseAPIListenAddress := apiListenAddress
	apiListenAddress = func(c *instance.Config) string {
		if addr := baseAPIListenAddress(c); addr != instance.DefaultAPIListenAddress {
			return addr
		}
		return hermeticEphemeralListen
	}

	disableGitFsyncForTests()
	disableJournalFsyncForTests()
	runTerminalWaitTimeout = suiteRunWaitTimeout

	os.Exit(m.Run())
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
	_ = os.Setenv("GIT_CONFIG_KEY_"+strconv.Itoa(n), "core.fsync")
	_ = os.Setenv("GIT_CONFIG_VALUE_"+strconv.Itoa(n), "none")
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
