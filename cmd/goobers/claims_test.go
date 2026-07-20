package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

func seedClaims(t *testing.T, root string, now time.Time) {
	t.Helper()
	l := instance.NewLayout(root)
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationAdminRelease, func() error {
		for _, claim := range []struct {
			itemID   string
			runID    string
			workflow string
			claimed  time.Time
		}{
			{itemID: "issue-9", runID: "run-b", workflow: "implement", claimed: now},
			{itemID: "issue-8", runID: "run-a", workflow: "curate", claimed: now.Add(-2 * time.Hour)},
		} {
			ledgerWithClock, err := localscheduler.OpenClaimLedger(
				filepath.Join(l.SchedulerDir(), claimLedgerFileName),
				localscheduler.WithLedgerClock(func() time.Time { return claim.claimed }),
			)
			if err != nil {
				return err
			}
			if ok, _, err := ledgerWithClock.Claim(claim.itemID, claim.runID, claim.workflow, time.Hour); err != nil {
				return err
			} else if !ok {
				return fmt.Errorf("claim %s was refused", claim.itemID)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestClaimsListStandaloneAndStaleFilter(t *testing.T) {
	root := initDemo(t)
	seedClaims(t, root, time.Now())

	code, stdout, stderr := runArgs(t, "claims", "list", root)
	if code != 0 {
		t.Fatalf("list: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{"issue-8", "run-a", "curate", "issue-9", "run-b", "implement", "CLAIMED AT", "EXPIRES AT"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout=%q, want %q", stdout, want)
		}
	}
	if strings.Index(stdout, "issue-8") > strings.Index(stdout, "issue-9") {
		t.Fatalf("claims are not sorted by item id: %q", stdout)
	}

	code, stdout, stderr = runArgs(t, "claims", "list", "--stale", "--json", root)
	if code != 0 {
		t.Fatalf("stale list: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var entries []localscheduler.ClaimEntry
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
		t.Fatalf("decode stale JSON: %v\n%s", err, stdout)
	}
	if len(entries) != 1 || entries[0].ItemID != "issue-8" {
		t.Fatalf("stale entries=%+v, want only issue-8", entries)
	}
}

func TestClaimsReleaseStandaloneIsDistinctlyJournaled(t *testing.T) {
	root := initDemo(t)
	seedClaims(t, root, time.Now())

	code, stdout, stderr := runArgs(t, "claims", "release", "issue-9", root)
	if code != 0 {
		t.Fatalf("release: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "was held by run run-b") {
		t.Fatalf("stdout=%q, want prior holder", stdout)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(instance.NewLayout(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, held := ledger.Lookup("issue-9"); held {
		t.Fatal("issue-9 is still claimed")
	}
	events, err := journal.ReadInstanceLog(instance.NewLayout(root).SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventClaimForceReleased && event.Name == "issue-9" && event.RunID == "run-b" {
			return
		}
	}
	t.Fatalf("force-release event not found: %+v", events)
}

func TestClaimsReleaseUnknownItemIsBusinessError(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "claims", "release", "missing", root)
	if code != 1 || !strings.Contains(stderr, "no claim for item") {
		t.Fatalf("code=%d stderr=%q, want no-claim business error", code, stderr)
	}
}

func TestClaimAdminDelegationRoundTrip(t *testing.T) {
	root := initDemo(t)
	seedClaims(t, root, time.Now())
	schedulerDir := instance.NewLayout(root).SchedulerDir()
	log, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()

	requestID, err := writeClaimAdminRequest(schedulerDir, claimAdminRequest{
		Operation: claimAdminOperationRelease,
		ItemID:    "issue-9",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sweepPendingClaimAdminRequests(schedulerDir, log, time.Now); err != nil {
		t.Fatal(err)
	}
	resp, err := pollClaimAdminResponse(context.Background(), schedulerDir, requestID, testResponseWait)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Released == nil || resp.Released.ItemID != "issue-9" || resp.Released.RunID != "run-b" {
		t.Fatalf("response=%+v", resp)
	}
}

func TestClaimsCommandsDelegateToLiveDaemon(t *testing.T) {
	previousInterval := delegationSweepInterval
	delegationSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() { delegationSweepInterval = previousInterval })

	root := initDeterministicDemo(t)
	seedClaims(t, root, time.Now())
	l := instance.NewLayout(root)

	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, []string{root}, started, &stderr) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("runUpContext did not stop")
		}
	})
	select {
	case <-started.started:
	case code := <-done:
		t.Fatalf("daemon exited before startup: code=%d stderr=%q", code, stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	code, stdout, commandStderr := runArgs(t, "claims", "list", root)
	if code != 0 || !strings.Contains(stdout, "issue-9") {
		t.Fatalf("live list: code=%d stdout=%q stderr=%q", code, stdout, commandStderr)
	}
	code, stdout, commandStderr = runArgs(t, "claims", "release", "issue-9", root)
	if code != 0 {
		t.Fatalf("live release: code=%d stdout=%q stderr=%q", code, stdout, commandStderr)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, held := ledger.Lookup("issue-9"); held {
		t.Fatal("live daemon did not release issue-9")
	}
}
