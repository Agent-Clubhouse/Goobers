package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// TestRunGitHubProgressRejectsNoWait pins the mutex the reviewer flagged: the
// --github-progress publishing loop lives inside the wait, so combining it
// with --no-wait would silently start a run that never publishes the terminal
// contract. The parser must reject the combination before either loop starts.
func TestRunGitHubProgressRejectsNoWait(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "run", "--github-progress", "--no-wait", "default-implement", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "--github-progress cannot be combined with --no-wait") {
		t.Fatalf("stderr = %q, want mutex error", stderr)
	}
}

// TestRunGitHubProgressRejectsRemoteAPI pins the second mutex the reviewer
// flagged: --github-progress projects events by walking the local instance
// journal, so submitting the trigger to a remote daemon via --api (or the
// $GOOBERS_DAEMON_API fallback) can never publish a Check Run. Silently
// dropping the flag on that path would leave the caller with the impression
// that live progress is being published when no publisher is running at all,
// and would also skip the --github-progress + --no-wait guard. Reject the
// combination with the same shape as --no-wait so the flag never no-ops.
func TestRunGitHubProgressRejectsRemoteAPI(t *testing.T) {
	// --api takes precedence over $GOOBERS_DAEMON_API, but clear the env
	// fallback so the assertion attributes the rejection to --api.
	t.Setenv("GOOBERS_DAEMON_API", "")
	code, _, stderr := runArgs(t, "run", "--github-progress", "--api", "https://daemon.example", "default-implement")
	if code != 2 {
		t.Fatalf("code = %d, want 2; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "--github-progress cannot be combined with --api") {
		t.Fatalf("stderr = %q, want --api mutex error", stderr)
	}
}

// TestRunGitHubProgressRejectsRemoteAPIViaEnv covers the second reachable
// path: $GOOBERS_DAEMON_API also makes the run remote (runRemoteTrigger),
// so the same mutex must fire even without an explicit --api flag.
func TestRunGitHubProgressRejectsRemoteAPIViaEnv(t *testing.T) {
	t.Setenv("GOOBERS_DAEMON_API", "https://daemon.example")
	code, _, stderr := runArgs(t, "run", "--github-progress", "default-implement")
	if code != 2 {
		t.Fatalf("code = %d, want 2; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "--github-progress cannot be combined with --api") {
		t.Fatalf("stderr = %q, want --api mutex error", stderr)
	}
}

// TestRunGitHubProgressRequiresActionsEnvironment pins the environment probe
// that fails fast when a caller passes --github-progress outside a hosted
// Actions workflow: with GITHUB_TOKEN and friends unset the run must not
// even start, so we do not create a Check Run with a bogus identity.
func TestRunGitHubProgressRequiresActionsEnvironment(t *testing.T) {
	for _, key := range []string{"GITHUB_TOKEN", "GITHUB_REPOSITORY", "GITHUB_SHA", "GITHUB_RUN_ID"} {
		t.Setenv(key, "")
	}
	root := initDemo(t)
	code, _, stderr := runArgs(t, "run", "--github-progress", "default-implement", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2; stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"github progress requires",
		"GITHUB_REPOSITORY",
		"GITHUB_SHA",
		"GITHUB_RUN_ID",
		"GITHUB_TOKEN",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to mention %q", stderr, want)
		}
	}
}

// TestRunFlagArgsPassesGithubProgressForms covers the four spellings the
// standard flag package would otherwise stop parsing at once the workflow
// positional appears. All four must survive so late-flag placement is a
// portable invocation shape (needed for shell-driven CI wrappers).
func TestRunFlagArgsPassesGithubProgressForms(t *testing.T) {
	for _, form := range []string{"--github-progress", "-github-progress", "--github-progress=true", "-github-progress=true"} {
		t.Run(form, func(t *testing.T) {
			got := runFlagArgs([]string{"deploy", form, "root"})
			want := []string{form, "deploy", "root"}
			if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("runFlagArgs(%q) = %q, want %q", form, got, want)
			}
		})
	}
}

// TestRunWaitReporterPublishFailedIsOneShot pins the degraded-path contract:
// when the hosted-progress publisher's first attempt fails the reporter must
// warn exactly once and continue observing without publishing. Otherwise a
// warm publisher failure (rate limit, transient 5xx) would emit one warning
// per journal event and drown out the actual run progress.
func TestRunWaitReporterPublishFailedIsOneShot(t *testing.T) {
	var progress synchronizedBuffer
	reporter := newRunWaitReporter("degraded", &progress)
	reporter.publishContext = context.Background()
	reporter.publish = func(context.Context, []journal.Event) error {
		return errors.New("stub publisher failure")
	}
	started := reporter.lastHeartbeat
	reporter.observe([]journal.Event{{
		Seq: 1, Type: journal.EventRunStarted, Time: started,
	}}, started)
	reporter.observe([]journal.Event{{
		Seq: 2, Type: journal.EventStageStarted, Stage: "build", Attempt: 1,
		Time: started,
	}}, started)

	output := progress.String()
	if got := strings.Count(output, "GitHub progress publishing stopped"); got != 1 {
		t.Fatalf("degraded warnings = %d, want exactly one:\n%s", got, output)
	}
	if !reporter.publishFailed {
		t.Fatal("publishFailed latch must remain set after a failed publish")
	}
}

// TestRunWaitReporterFinalizeIsBestEffort pins the check-run lifecycle
// guarantee: on abnormal exit (context cancelled, wait error) the reporter
// closes any in-flight Check Run via the finalize hook, and a nil hook (the
// default when --github-progress is off) is a no-op — the CLI exit path
// must not depend on hosted progress being wired up.
func TestRunWaitReporterFinalizeIsBestEffort(t *testing.T) {
	t.Run("nil finalize is a no-op", func(t *testing.T) {
		reporter := newRunWaitReporter("no-hosted-progress", nil)
		reporter.Finalize(nil)
	})

	t.Run("failure surfaces as one-shot warning", func(t *testing.T) {
		var progress synchronizedBuffer
		reporter := newRunWaitReporter("finalize-warn", &progress)
		reporter.finalize = func(context.Context, error) error {
			return errors.New("stub finalize failure")
		}
		reporter.Finalize(errors.New("wait failed"))
		reporter.Finalize(errors.New("wait failed again"))
		if got := strings.Count(progress.String(), "GitHub progress finalize failed"); got != 1 {
			t.Fatalf("finalize warnings = %d, want exactly one:\n%s", got, progress.String())
		}
	})

	t.Run("propagates cancellation cause", func(t *testing.T) {
		var seenErr error
		reporter := newRunWaitReporter("finalize-cause", nil)
		reporter.finalize = func(_ context.Context, waitErr error) error {
			seenErr = waitErr
			return nil
		}
		reporter.Finalize(context.Canceled)
		if !errors.Is(seenErr, context.Canceled) {
			t.Fatalf("finalize received %v, want context.Canceled", seenErr)
		}
	})
}
