package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/selfupdate"
)

func newTestChecker(t *testing.T, check func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error)) *updateChecker {
	t.Helper()
	return &updateChecker{
		root:     t.TempDir(),
		settings: instance.UpdateCheckConfig{},
		interval: time.Hour,
		current:  "v0.4.0",
		check:    check,
		pending:  &updatePending{},
	}
}

// An operator who does not act on a release must not be told about that same
// release on every tick; a NEW release must still be announced.
func TestUpdateCheckerAnnouncesOnlyOnChange(t *testing.T) {
	latest := "v0.5.0"
	checker := newTestChecker(t, func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		return selfupdate.CheckResult{
			CurrentVersion: "v0.4.0", LatestVersion: latest, UpdateAvailable: true, Channel: selfupdate.ChannelStable,
		}, nil
	})

	first, ok := checker.once(context.Background())
	if !ok || !strings.Contains(first.notice, "v0.5.0") {
		t.Fatalf("first check = %+v, ok = %t, want a v0.5.0 notice", first, ok)
	}
	if _, ok := checker.once(context.Background()); ok {
		t.Error("second check re-announced the same version")
	}
	latest = "v0.6.0"
	third, ok := checker.once(context.Background())
	if !ok || !strings.Contains(third.notice, "v0.6.0") {
		t.Errorf("third check = %+v, ok = %t, want a v0.6.0 notice", third, ok)
	}
}

func TestUpdateCheckerSilentWhenCurrent(t *testing.T) {
	checker := newTestChecker(t, func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		return selfupdate.CheckResult{CurrentVersion: "v0.4.0", LatestVersion: "v0.4.0"}, nil
	})
	if result, ok := checker.once(context.Background()); ok {
		t.Errorf("once() = %+v, want silence for a current build", result)
	}
}

// A dev build has nothing to compare, which is the normal state of a
// development daemon rather than something to report.
func TestUpdateCheckerSilentForDevBuild(t *testing.T) {
	checker := newTestChecker(t, func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		return selfupdate.CheckResult{}, selfupdate.ErrVersionNotComparable
	})
	if result, ok := checker.once(context.Background()); ok {
		t.Errorf("once() = %+v, want silence for an incomparable build", result)
	}
}

// A release source that is down must warn once, not once per tick, and must
// never look like a daemon failure.
func TestUpdateCheckerWarnsOnceOnFailure(t *testing.T) {
	checker := newTestChecker(t, func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		return selfupdate.CheckResult{}, errors.New("dial tcp: connection refused")
	})
	first, ok := checker.once(context.Background())
	if !ok || first.warning == "" || first.notice != "" {
		t.Fatalf("first check = %+v, ok = %t, want a warning and no notice", first, ok)
	}
	if _, ok := checker.once(context.Background()); ok {
		t.Error("second failing check warned again")
	}
}

// The cache backs `goobers status`, so a successful check must leave one
// behind even when there is nothing to announce.
func TestUpdateCheckerCachesResult(t *testing.T) {
	checker := newTestChecker(t, func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		return selfupdate.CheckResult{CurrentVersion: "v0.4.0", LatestVersion: "v0.4.0", Channel: selfupdate.ChannelStable}, nil
	})
	if _, ok := checker.once(context.Background()); ok {
		t.Fatal("once() announced for a current build")
	}
	cached, err := selfupdate.ReadCheck(checker.root)
	if err != nil {
		t.Fatalf("ReadCheck() error = %v", err)
	}
	if cached.LatestVersion != "v0.4.0" {
		t.Errorf("cached LatestVersion = %q, want v0.4.0", cached.LatestVersion)
	}
}

func TestUpdateCheckerPassesConfiguredChannel(t *testing.T) {
	var seen selfupdate.CheckOptions
	checker := newTestChecker(t, func(_ context.Context, opts selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		seen = opts
		return selfupdate.CheckResult{CurrentVersion: "v0.4.0", LatestVersion: "v0.4.0"}, nil
	})
	checker.settings = instance.UpdateCheckConfig{Channel: selfupdate.ChannelPrerelease, Owner: "acme", Repository: "app"}
	checker.once(context.Background())
	if seen.Channel != selfupdate.ChannelPrerelease || seen.Owner != "acme" || seen.Repository != "app" {
		t.Errorf("check options = %+v, want the configured channel and release source", seen)
	}
}

// `enabled: false` must produce no goroutine and no request at all.
func TestStartUpdateCheckDisabled(t *testing.T) {
	disabled := false
	cfg := &instance.Config{UpdateCheck: &instance.UpdateCheckConfig{Enabled: &disabled}}
	var stderr bytes.Buffer
	results, done, pending := startUpdateCheck(context.Background(), t.TempDir(), cfg, &stderr)
	if results != nil {
		t.Error("startUpdateCheck(disabled) returned a results channel, want nil so it never fires")
	}
	// done comes back already closed rather than nil so the daemon's shutdown
	// join needs no special case for a disabled check.
	select {
	case <-done:
	default:
		t.Error("startUpdateCheck(disabled) done channel is not closed")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	// A disabled check still hands back a holder so the heartbeat needs no
	// special case; it simply never reports a version.
	if pending == nil {
		t.Error("startUpdateCheck(disabled) returned a nil pending holder")
	} else if clause := pending.clause(); clause != "" {
		t.Errorf("pending.clause() = %q for a disabled check, want empty", clause)
	}
}

func TestStartUpdateCheckStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := &instance.Config{}
	var stderr bytes.Buffer
	_, done, _ := startUpdateCheck(ctx, t.TempDir(), cfg, &stderr)
	if done == nil {
		t.Fatal("startUpdateCheck() returned a nil done channel for an enabled check")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("update checker did not stop on context cancellation")
	}
}

// The heartbeat clause is a CONDITION with a different lifetime from the
// announcement: it must persist across ticks that do not re-announce, and it
// must clear the moment the running build catches up.
func TestUpdatePendingConditionLifetime(t *testing.T) {
	latest := "v0.5.0"
	available := true
	checker := newTestChecker(t, func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		return selfupdate.CheckResult{
			CurrentVersion: "v0.4.0", LatestVersion: latest, UpdateAvailable: available,
		}, nil
	})

	checker.once(context.Background())
	if got := checker.pending.clause(); got != "; update v0.5.0 available" {
		t.Fatalf("clause after first check = %q", got)
	}

	// A second tick re-announces nothing, but the condition still holds.
	if _, ok := checker.once(context.Background()); ok {
		t.Error("second check re-announced")
	}
	if got := checker.pending.clause(); got != "; update v0.5.0 available" {
		t.Errorf("clause after a non-announcing tick = %q, want it to persist", got)
	}

	// The operator updates: the condition must clear even though this tick
	// announces nothing either.
	available = false
	checker.once(context.Background())
	if got := checker.pending.clause(); got != "" {
		t.Errorf("clause after the build caught up = %q, want empty", got)
	}
}

// A failing check must not clear a condition established by an earlier
// successful one: an unreachable release source is not evidence the operator
// has updated.
func TestUpdatePendingSurvivesCheckFailure(t *testing.T) {
	fail := false
	checker := newTestChecker(t, func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		if fail {
			return selfupdate.CheckResult{}, errors.New("dial tcp: connection refused")
		}
		return selfupdate.CheckResult{CurrentVersion: "v0.4.0", LatestVersion: "v0.5.0", UpdateAvailable: true}, nil
	})
	checker.once(context.Background())
	fail = true
	checker.once(context.Background())
	if got := checker.pending.clause(); got != "; update v0.5.0 available" {
		t.Errorf("clause after a failed check = %q, want the earlier condition retained", got)
	}
}

func TestUpdatePendingClause(t *testing.T) {
	var pending updatePending
	if got := pending.clause(); got != "" {
		t.Errorf("zero-value clause() = %q, want empty", got)
	}
	pending.set("v1.2.3")
	if got := pending.clause(); got != "; update v1.2.3 available" {
		t.Errorf("clause() = %q", got)
	}
	pending.set("")
	if got := pending.clause(); got != "" {
		t.Errorf("cleared clause() = %q, want empty", got)
	}
	// A nil holder is valid for a heartbeat with no checker wired up.
	if got := updateClause(nil); got != "" {
		t.Errorf("updateClause(nil) = %q, want empty", got)
	}
}

// The point of the clause is that it survives in a stream the one-shot notice
// scrolls out of, so assert it reaches the rendered heartbeat line itself —
// and that a current build leaves that line byte-identical to before.
func TestHeartbeatCarriesUpdateClause(t *testing.T) {
	tests := []struct {
		name    string
		pending *updatePending
		want    string
		absent  string
	}{
		{name: "update pending", pending: pendingAt("v0.5.0"), want: "; update v0.5.0 available"},
		{name: "build current", pending: &updatePending{}, absent: "update"},
		{name: "no checker wired", pending: nil, absent: "update"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			log, _, err := journal.OpenInstanceLog(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = log.Close() })
			tail, err := journal.OpenInstanceLogTail(dir)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			stdout := newDaemonOutput()
			done := make(chan struct{})
			go emitHeartbeats(ctx, stdout, dir, 1, tail, nil, 10*time.Millisecond, test.pending, done)
			select {
			case <-stdout.heartbeat:
			case <-time.After(10 * time.Second):
				t.Error("heartbeat was not emitted")
			}
			cancel()
			<-done

			output := stdout.String()
			if test.want != "" && !strings.Contains(output, test.want) {
				t.Errorf("heartbeat = %q, want it to contain %q", output, test.want)
			}
			if test.absent != "" && strings.Contains(output, test.absent) {
				t.Errorf("heartbeat = %q, want no %q clause", output, test.absent)
			}
		})
	}
}

func pendingAt(version string) *updatePending {
	pending := &updatePending{}
	pending.set(version)
	return pending
}

// A cache write that fails leaves the heartbeat correct and `goobers status`
// stale. That divergence is narrow but real, so the warning must name it
// rather than reporting a bare write error an operator cannot act on.
func TestUpdateCheckCacheFailureNamesTheDivergence(t *testing.T) {
	checker := newTestChecker(t, func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		return selfupdate.CheckResult{CurrentVersion: "v0.4.0", LatestVersion: "v0.5.0", UpdateAvailable: true}, nil
	})
	// An unwritable root is the real-world shape of this: WriteCheck cannot
	// create <root>/updates/ under a file.
	blocked := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	checker.root = blocked

	result, ok := checker.once(context.Background())
	if !ok || result.warning == "" {
		t.Fatalf("once() = %+v, ok = %t, want a warning", result, ok)
	}
	if !strings.Contains(result.warning, "goobers status") {
		t.Errorf("warning = %q, want it to name the stale status surface", result.warning)
	}
	// The heartbeat keeps the accurate condition despite the failed write.
	if got := checker.pending.clause(); got != "; update v0.5.0 available" {
		t.Errorf("clause after a failed cache write = %q, want the condition retained", got)
	}
}
