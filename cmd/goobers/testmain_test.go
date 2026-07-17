package main

import (
	"os"
	"strconv"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

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

	os.Exit(m.Run())
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
	os.Setenv("GIT_CONFIG_KEY_"+strconv.Itoa(n), "core.fsync")
	os.Setenv("GIT_CONFIG_VALUE_"+strconv.Itoa(n), "none")
	os.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(n+1))
}
