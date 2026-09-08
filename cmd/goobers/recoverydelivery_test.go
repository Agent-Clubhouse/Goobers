package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

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
