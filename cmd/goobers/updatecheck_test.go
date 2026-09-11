package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
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
	results, done := startUpdateCheck(context.Background(), t.TempDir(), cfg, &stderr)
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
}

func TestStartUpdateCheckStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := &instance.Config{}
	var stderr bytes.Buffer
	_, done := startUpdateCheck(ctx, t.TempDir(), cfg, &stderr)
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
