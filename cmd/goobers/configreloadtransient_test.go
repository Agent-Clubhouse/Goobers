package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

func TestConfigReloaderSchedulesTransientRetryWithBackoff(t *testing.T) {
	now := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	transient := fmt.Errorf("initialize gaggle %q runtime: %w", "g", fmt.Errorf("verify: %w", credentials.ErrTransientProvider))
	r := &configReloader{}

	err := r.scheduleTransientRetry("d1", transient, now)
	if !errors.Is(err, credentials.ErrTransientProvider) || !strings.Contains(err.Error(), "retrying in 30s") {
		t.Fatalf("scheduled error = %v, want transient with retry note", err)
	}
	if r.takeTransientRetry("d1", now.Add(29*time.Second)) {
		t.Fatal("retry taken before its backoff elapsed")
	}
	if r.takeTransientRetry("d2", now.Add(time.Hour)) {
		t.Fatal("retry taken for a different digest")
	}
	if !r.takeTransientRetry("d1", now.Add(30*time.Second)) {
		t.Fatal("retry not taken once due")
	}
	if r.takeTransientRetry("d1", now.Add(time.Hour)) {
		t.Fatal("a consumed retry must not fire again until rescheduled")
	}

	// Each further transient rejection of the same digest doubles, capped.
	at := now
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		_ = r.scheduleTransientRetry("d1", transient, at)
		if r.transientRetry.backoff != want {
			t.Fatalf("backoff = %s, want %s", r.transientRetry.backoff, want)
		}
		at = r.transientRetry.at
	}
	// A new digest starts over; a deterministic failure clears the schedule.
	_ = r.scheduleTransientRetry("d2", transient, at)
	if r.transientRetry.backoff != transientReloadRetryInitial {
		t.Fatalf("new digest backoff = %s, want %s", r.transientRetry.backoff, transientReloadRetryInitial)
	}
	deterministic := errors.New("config directory invalid")
	if got := r.scheduleTransientRetry("d2", deterministic, at); got.Error() != deterministic.Error() || errors.Is(got, credentials.ErrTransientProvider) {
		t.Fatalf("deterministic error rewritten: %v", got)
	}
	if r.transientRetry != (transientReloadRetry{}) {
		t.Fatalf("deterministic failure left retry state %+v", r.transientRetry)
	}
}

// #5596: an unchanged digest whose rejection was transient is re-evaluated
// once its retry is due; before the fix poll returned early forever.
func TestConfigReloaderPollReevaluatesDigestWithDueTransientRetry(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.ConfigDir(), "candidate.yaml"), []byte("kind: rejected\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := configDirectoryDigest(layout.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	now := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	r := &configReloader{
		layout:         layout,
		setup:          &schedulerSetup{InstanceLog: instanceLog},
		appliedDigest:  "applied",
		observedDigest: digest,
		rejectedDigest: digest,
		transientRetry: transientReloadRetry{digest: digest, at: now.Add(time.Minute), backoff: time.Minute},
	}
	rejections := func() int {
		events, err := journal.ReadInstanceLog(layout.SchedulerDir())
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, event := range events {
			if event.Type == journal.EventConfigReloadRejected {
				count++
			}
		}
		return count
	}

	if err := r.poll(now); err != nil {
		t.Fatal(err)
	}
	if got := rejections(); got != 0 {
		t.Fatalf("rejections before the retry is due = %d, want 0", got)
	}
	if err := r.poll(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := rejections(); got != 1 {
		t.Fatalf("rejections once the retry is due = %d, want 1 (digest re-evaluated)", got)
	}
	// This attempt failed deterministically, so nothing more is scheduled.
	if err := r.poll(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := rejections(); got != 1 {
		t.Fatalf("rejections after a deterministic retry failure = %d, want 1", got)
	}
}

func TestRetryTransientStartupWaitsOutTransientProviderFailures(t *testing.T) {
	previous := transientStartupRetry
	transientStartupRetry.initial = time.Millisecond
	transientStartupRetry.max = 2 * time.Millisecond
	transientStartupRetry.budget = time.Minute
	t.Cleanup(func() { transientStartupRetry = previous })
	transient := fmt.Errorf("build credential resolver: %w", fmt.Errorf("x: %w", credentials.ErrTransientProvider))

	calls := 0
	got, err := retryTransientStartup(context.Background(), io.Discard, func() (int, error) {
		calls++
		if calls < 3 {
			return 0, transient
		}
		return 7, nil
	})
	if err != nil || got != 7 || calls != 3 {
		t.Fatalf("retry = (%d, %v) after %d calls, want (7, nil) after 3", got, err, calls)
	}

	calls = 0
	deterministic := errors.New("config directory invalid")
	if _, err := retryTransientStartup(context.Background(), io.Discard, func() (int, error) {
		calls++
		return 0, deterministic
	}); !errors.Is(err, deterministic) || calls != 1 {
		t.Fatalf("deterministic failure = %v after %d calls, want immediate failure", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	transientStartupRetry.initial = time.Hour
	_, err = retryTransientStartup(ctx, io.Discard, func() (int, error) {
		cancel()
		return 0, transient
	})
	if !daemonStartupStoppedByShutdown(ctx, err) {
		t.Fatalf("cancelled wait = %v, want a clean shutdown stop", err)
	}

	transientStartupRetry.initial = time.Millisecond
	transientStartupRetry.budget = 0
	calls = 0
	if _, err := retryTransientStartup(context.Background(), io.Discard, func() (int, error) {
		calls++
		return 0, transient
	}); !errors.Is(err, credentials.ErrTransientProvider) || calls != 1 {
		t.Fatalf("spent budget = %v after %d calls, want the transient failure", err, calls)
	}
}

func TestConfigReloadRejectionStatusLine(t *testing.T) {
	if got := configReloadRejectionStatusLine(readservice.SchedulerStatus{}); got != "" {
		t.Fatalf("no rejection line = %q, want empty", got)
	}
	got := configReloadRejectionStatusLine(readservice.SchedulerStatus{ConfigReloadRejection: &readservice.ConfigReloadRejectionStatus{
		At: time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC), Digest: "0123456789abcdef", Message: "verify GitHub identity: status 403 Forbidden",
	}})
	for _, want := range []string{"config reload rejected at 2026-09-24T03:00:00Z", "candidate 0123456789ab", "status 403 Forbidden"} {
		if !strings.Contains(got, want) {
			t.Fatalf("line = %q, want %q", got, want)
		}
	}
}
