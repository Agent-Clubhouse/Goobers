package claimsclient

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

func executionTestClaim(now time.Time) Entry {
	return Entry{Gaggle: "g", Provider: "github", ExternalID: "42", RunID: "run", ClaimedAt: now,
		ExpiresAt: now.Add(time.Minute), SharedDeadline: now.Add(time.Minute),
		SharedOwner: sharedclaim.Owner{Instance: "instance", Run: "run", Token: "token"}}
}

func TestExecutionClaimsMissingEntryDoesNotEraseDeadline(t *testing.T) {
	now := time.Now()
	entry := executionTestClaim(now)
	state := executionClaims{runID: "run", held: make(map[Key]Entry)}
	if err := state.update(Listing{Entries: []Entry{entry}}, now); err != nil {
		t.Fatal(err)
	}
	if err := state.update(Listing{}, now.Add(time.Second)); err != nil || !state.deadline().Equal(entry.SharedDeadline) {
		t.Fatalf("missing ownership erased deadline: %v %s", err, state.deadline())
	}
	entry.SharedDeadline = now.Add(time.Hour)
	entry.ExpiresAt = entry.SharedDeadline
	if err := state.update(Listing{Entries: []Entry{entry}}, now.Add(time.Minute)); !errors.Is(err, ErrSharedExecutionExpired) {
		t.Fatalf("late renewal resurrected execution: %v", err)
	}
}

func TestExecutionClaimsAdministrativeRevocationIsNotVoluntaryRelease(t *testing.T) {
	for _, history := range []bool{false, true} {
		now := time.Now()
		entry := executionTestClaim(now)
		entry.SharedRevoked = true
		listing := Listing{Entries: []Entry{entry}}
		if history {
			released := now.Add(time.Second)
			entry.ReleasedAt = &released
			listing = Listing{History: []Entry{entry}}
		}
		state := executionClaims{runID: "run", held: make(map[Key]Entry)}
		if err := state.update(listing, now); !errors.Is(err, ErrSharedExecutionExpired) {
			t.Fatalf("revocation enabled further execution: history=%v err=%v", history, err)
		}
	}
}

func TestExecutionClaimsReleaseAndRenewalEvidence(t *testing.T) {
	for _, kind := range []string{"early-release", "expired-release", "renewal", "different-owner", "foreign-run", "local-claim"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			entry := executionTestClaim(now)
			state := executionClaims{runID: "run", held: make(map[Key]Entry)}
			if err := state.update(Listing{Entries: []Entry{entry}}, now); err != nil {
				t.Fatal(err)
			}
			listing := Listing{}
			want := entry.SharedDeadline
			wantError := false
			switch kind {
			case "early-release":
				released := now.Add(time.Second)
				entry.ReleasedAt = &released
				listing.History = []Entry{entry}
				want = time.Time{}
			case "expired-release":
				released := entry.SharedDeadline
				entry.ReleasedAt = &released
				listing.History = []Entry{entry}
				wantError = true
			case "renewal":
				entry.ExpiresAt = now.Add(time.Hour)
				entry.SharedDeadline = entry.ExpiresAt
				listing.Entries = []Entry{entry}
				want = entry.SharedDeadline
			case "different-owner":
				entry.SharedOwner.Token = "replacement"
				listing.Entries = []Entry{entry}
				wantError = true
			case "foreign-run":
				entry.RunID = "other"
				listing.Entries = []Entry{entry}
			case "local-claim":
				entry.SharedDeadline = time.Time{}
				listing.Entries = []Entry{entry}
			}
			err := state.update(listing, now.Add(2*time.Second))
			if (err != nil) != wantError || (!wantError && !state.deadline().Equal(want)) {
				t.Fatalf("deadline=%s want=%s err=%v", state.deadline(), want, err)
			}
		})
	}
}

func TestExecutionFenceDeadlineSurvivesHungSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		entry := executionTestClaim(started)
		entry.SharedDeadline = started.Add(300 * time.Millisecond)
		entry.ExpiresAt = entry.SharedDeadline
		first := true
		blocked, exited := make(chan struct{}), make(chan struct{})
		ctx, stop, err := StartExecutionFenceWithOptions(t.Context(), "run", func(ctx context.Context) (Listing, error) {
			if first {
				first = false
				return Listing{Entries: []Entry{entry}}, nil
			}
			close(blocked)
			<-ctx.Done()
			close(exited)
			return Listing{}, ctx.Err()
		}, ExecutionFenceOptions{SnapshotTimeout: time.Minute})
		defer stop()
		if err != nil {
			t.Fatal(err)
		}
		<-blocked
		<-ctx.Done()
		if !errors.Is(context.Cause(ctx), ErrSharedExecutionExpired) || time.Since(started) != 300*time.Millisecond {
			t.Fatalf("observation budget extended lease: elapsed=%s cause=%v", time.Since(started), context.Cause(ctx))
		}
		<-exited
	})
}

func TestExecutionFenceObservesClaimsAcquiredDuringExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := true
		ctx, stop, err := StartExecutionFence(t.Context(), "run", func(context.Context) (Listing, error) {
			if first {
				first = false
				return Listing{}, nil
			}
			entry := executionTestClaim(time.Now().Add(-time.Hour))
			return Listing{Entries: []Entry{entry}}, nil
		})
		defer stop()
		if err != nil {
			t.Fatal(err)
		}
		<-ctx.Done()
		if !errors.Is(context.Cause(ctx), ErrSharedExecutionExpired) {
			t.Fatal(context.Cause(ctx))
		}
	})
}

func TestExecutionFenceStalledReadWithoutKnownClaimFailsClosed(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				first := true
				unblock, exited := make(chan struct{}), make(chan struct{})
				var readStarted time.Time
				ctx, stop, err := StartExecutionFenceWithOptions(t.Context(), "run", func(context.Context) (Listing, error) {
					if first {
						first = false
						return Listing{}, nil
					}
					readStarted = time.Now()
					// Ignore cancellation to prove the watchdog acts independently.
					<-unblock
					close(exited)
					return Listing{}, nil
				}, ExecutionFenceOptions{SnapshotTimeout: timeout})
				defer stop()
				if err != nil {
					t.Fatal(err)
				}
				<-ctx.Done()
				synctest.Wait()
				want := timeout
				if want == 0 {
					want = DefaultExecutionSnapshotTimeout
				}
				if !errors.Is(context.Cause(ctx), ErrSharedExecutionExpired) || time.Since(readStarted) != want {
					t.Errorf("watchdog elapsed=%s want=%s cause=%v", time.Since(readStarted), want, context.Cause(ctx))
				}
				close(unblock)
				<-exited
			})
		})
	}
}

func TestExecutionFenceAllowsHealthyObservationWithinRouteBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop, err := StartExecutionFence(t.Context(), "run", func(context.Context) (Listing, error) {
			// A healthy response beyond the old one-second watchdog must be allowed.
			time.Sleep(2 * time.Second)
			return Listing{}, nil
		})
		defer stop()
		if err != nil || ctx.Err() != nil {
			t.Fatalf("healthy observation was falsely fenced: err=%v cause=%v", err, context.Cause(ctx))
		}
	})
}
