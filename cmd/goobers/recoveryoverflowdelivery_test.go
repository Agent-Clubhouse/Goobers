package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

// Exercise the actual claims-authenticated route and pod restore entrypoint
// while the selected record has no bundle. Pending must survive the entire
// transport, without becoming "absent" or reaching the archive consumer.
func TestRecoveryOverflowClaimsRestoreReportsPromotionState(t *testing.T) {
	for _, mode := range []string{"awaiting", "full", "dry-run", "disabled", "released", "abandoned", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			layout := instance.NewLayout(initDemo(t))
			now := time.Now().UTC()
			repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "team", Name: "repo"}
			path := seedRecoverySelection(t, layout, repo, "overflow-source", "7", now.Add(-time.Hour), now.Add(time.Hour), true)
			record, err := recovery.ReadRecord(path)
			if err != nil {
				t.Fatal(err)
			}
			record.ArchiveBytes, record.ArchiveDigest, record.ArchiveFormat = 0, "", ""
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			overflowPath := filepath.Join(recoveryOverflowRoot(layout), filepath.Base(filepath.Dir(path)), recovery.RecordFileName)
			if err := os.MkdirAll(filepath.Dir(overflowPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(overflowPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Dir(path)); err != nil {
				t.Fatal(err)
			}
			if _, err := recovery.ReadOverflowRecord(overflowPath); err != nil {
				t.Fatal(err)
			}
			cfg, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			want := recovery.PromotionAwaiting
			switch mode {
			case "full":
				cfg.Retention.Recovery = &instance.RecoverySnapshotConfig{MaxSnapshots: 1}
				seedRecoverySelection(t, layout, repo, "other-source", "8", now.Add(-time.Hour), now.Add(time.Hour), true)
				want = recovery.PromotionCapacity
			case "dry-run":
				cfg.Retention.DryRun = true
				want = recovery.PromotionDryRun
			case "disabled":
				disabled := false
				cfg.Retention.Enabled = &disabled
				want = recovery.PromotionDisabled
			}
			if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
				t.Fatal(err)
			}
			const receiving = "receiving-run"
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
			if err != nil {
				t.Fatal(err)
			}
			if ok, _, err := ledger.Claim("7", receiving, "implementation-recovery", time.Hour); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			seedItemRepositoryForTest(t, layout, receiving, "7", repo)
			if mode == "released" {
				if err := ledger.Release("7", receiving); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "abandoned" {
				event, err := recovery.AbandonedEvent(record)
				if err != nil {
					t.Fatal(err)
				}
				journalLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
				if err != nil {
					t.Fatal(err)
				}
				if err := journalLog.Append(event); err != nil {
					t.Fatal(err)
				}
				if err := journalLog.Close(); err != nil {
					t.Fatal(err)
				}
			}
			registry := podauth.NewRegistry()
			auth, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
			if err != nil {
				t.Fatal(err)
			}
			token, err := registry.MintScoped(receiving, time.Hour, podauth.ScopeClaims)
			if err != nil {
				t.Fatal(err)
			}
			service := recoveryDeliveryService{layout: layout, setup: &schedulerSetup{Config: cfg}}
			handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithRecoveryService(service))
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			t.Setenv(claimsclient.EnvEndpoint, server.URL)
			t.Setenv(claimsclient.EnvToken, token)
			t.Setenv(claimsclient.EnvRunID, receiving)
			key := repo.CanonicalKey()
			if mode == "foreign" {
				key = "github|||other|repo|"
			}
			_, err = restoreDownloadedRecovery(t.Context(), key, "7", func(string) (string, error) { t.Fatal("pending overflow reached archive consumer"); return "", nil })
			var pending *recovery.OverflowPendingError
			if mode == "released" || mode == "abandoned" || mode == "foreign" {
				if err == nil || errors.As(err, &pending) {
					t.Fatalf("unauthorized/abandoned state disclosed: %v", err)
				}
			} else if !errors.As(err, &pending) || pending.PromotionState != want {
				t.Fatalf("want overflow-pending state %s, got %v", want, err)
			}
			if current, err := recovery.ReadOverflowRecord(overflowPath); err != nil || current != record {
				t.Fatalf("delivery mutated overflow evidence: %+v %v", current, err)
			}
		})
	}
}
