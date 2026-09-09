package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

func TestRecoveryDeliveryServiceStreamsOnlyVerifiedClaimedState(t *testing.T) {
	for _, mode := range []string{"valid", "released", "busy-source", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			layout := instance.NewLayout(initDemo(t))
			now := time.Now().UTC()
			repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
			path := seedRecoverySelection(t, layout, repo, "source-run", "7", now.Add(-time.Hour), now.Add(time.Hour), true)
			record, err := recovery.ReadRecord(path)
			if err != nil {
				t.Fatal(err)
			}
			archive := []byte(fmt.Sprintf("# v3 git bundle\n@object-format=sha1\n%s %s\n\nfixture", record.SnapshotSHA, record.Ref))
			record.ArchiveBytes = int64(len(archive))
			record.ArchiveDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(archive))
			metadata, err := recovery.Encode(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, metadata, 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt" {
				archive[len(archive)-1] ^= 1
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), recovery.BundleFileName), archive, 0o600); err != nil {
				t.Fatal(err)
			}
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
			if err != nil {
				t.Fatal(err)
			}
			if ok, _, err := ledger.Claim("7", "receiving-run", "implementation-recovery", time.Hour); err != nil || !ok {
				t.Fatalf("claim: %t %v", ok, err)
			}
			seedItemRepositoryForTest(t, layout, "receiving-run", "7", repo)
			if mode == "released" {
				if err := ledger.Release("7", "receiving-run"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "busy-source" {
				run, _, err := journal.Recover(filepath.Join(layout.RunsDir(), "source-run"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = run.Close() }()
			}
			var out bytes.Buffer
			err = (recoveryDeliveryService{layout: layout}).StreamRecovery(context.Background(), "receiving-run", repo.CanonicalKey(), "7", &out)
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				got, err := recovery.ReceiveArchiveEnvelope(context.Background(), &out, t.TempDir(), 4096, nil)
				if err != nil || got != record {
					t.Fatalf("received wrong recovery: %+v %v", got, err)
				}
			} else if err == nil || out.Len() != 0 {
				t.Fatalf("unsafe delivery: bytes=%d err=%v", out.Len(), err)
			}
		})
	}
}

func TestRecoveryDeliveryRequiresCurrentSingleIssueLease(t *testing.T) {
	for _, mode := range []string{"live", "expired", "released", "multiple", "foreign-run", "foreign-repository"} {
		t.Run(mode, func(t *testing.T) {
			layout := instance.NewLayout(initDemo(t))
			now := time.Now().UTC()
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName), localscheduler.WithLedgerClock(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			const runID = "receiving-run"
			if ok, _, err := ledger.Claim("7", runID, "implementation-recovery", time.Hour); err != nil || !ok {
				t.Fatalf("seed claim: %t %v", ok, err)
			}
			repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
			seedItemRepositoryForTest(t, layout, runID, "7", repo)
			requestedRun, requestedKey, at := runID, repo.CanonicalKey(), now
			switch mode {
			case "expired":
				at = now.Add(time.Hour)
			case "released":
				if err := ledger.Release("7", runID); err != nil {
					t.Fatal(err)
				}
			case "multiple":
				if ok, _, err := ledger.Claim("8", runID, "implementation-recovery", time.Hour); err != nil || !ok {
					t.Fatalf("second claim: %t %v", ok, err)
				}
			case "foreign-run":
				requestedRun = "another-run"
			case "foreign-repository":
				requestedKey = "github|||another-team|repo|"
			}
			deadline, err := authorizeRecoveryDelivery(context.Background(), layout, requestedRun, requestedKey, "7", at)
			if mode == "live" {
				if err != nil || !deadline.Equal(now.Add(time.Hour)) {
					t.Fatalf("live lease refused: %s %v", deadline, err)
				}
			} else if err == nil || !deadline.IsZero() {
				t.Fatalf("unauthorized transfer admitted: %s %v", deadline, err)
			}
		})
	}
}

func TestRecoveryDeliveryAcknowledgementHoldsCurrentClaim(t *testing.T) {
	for _, mode := range []string{"live", "released", "ack-failure"} {
		t.Run(mode, func(t *testing.T) {
			layout := instance.NewLayout(initDemo(t))
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
			if err != nil {
				t.Fatal(err)
			}
			const runID = "publication-run"
			if ok, _, err := ledger.Claim("7", runID, "implementation", time.Hour); err != nil || !ok {
				t.Fatalf("claim: %t %v", ok, err)
			}
			repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
			seedItemRepositoryForTest(t, layout, runID, "7", repo)
			if mode == "released" {
				if err := ledger.Release("7", runID); err != nil {
					t.Fatal(err)
				}
			}
			ackFailure := errors.New("durable acknowledgement failed")
			called := false
			deadline, err := withAuthorizedRecoveryDelivery(context.Background(), layout, runID, repo.CanonicalKey(), "7", time.Now().UTC(), func() error {
				called = true
				// A competing release uses this same lock. It cannot enter between
				// authorization and the journal's durable acknowledgement.
				held, lockErr := lock.TryAcquire(filepath.Join(layout.SchedulerDir(), claimLockFileName))
				if held != nil {
					_ = held.Release()
				}
				if !errors.Is(lockErr, lock.ErrHeld) {
					t.Fatalf("acknowledgement did not hold claim lock: %v", lockErr)
				}
				if mode == "ack-failure" {
					return ackFailure
				}
				return nil
			})
			if mode == "live" {
				if err != nil || !called || deadline.IsZero() {
					t.Fatalf("live acknowledgement: called=%t deadline=%s err=%v", called, deadline, err)
				}
			} else if err == nil || !deadline.IsZero() || called != (mode == "ack-failure") {
				t.Fatalf("unsafe acknowledgement: called=%t deadline=%s err=%v", called, deadline, err)
			}
			if mode == "ack-failure" && !errors.Is(err, ackFailure) {
				t.Fatalf("lost acknowledgement error: %v", err)
			}
		})
	}
}
