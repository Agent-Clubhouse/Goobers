package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/internal/workbenchservice"
	"github.com/goobers/goobers/providers"
)

func TestResolutionLearnedScopeUsesExactTargetAndSharedClaimsLock(t *testing.T) {
	directory := t.TempDir()
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "issues"}
	record := blockedRecord{Repository: repo, ItemID: "42", Blockers: []string{"43"}, Reason: "waiting for dependency", RunID: "old-run", RecordedAt: time.Now().UTC()}
	records := map[string]blockedRecord{blockedRecordKey(repo, "42"): record}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, stateclient.KeyBlockedRecords)
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	scope := workbenchLearnedBlocks(directory)
	err = scope(t.Context(), repo, "42", func(ctx context.Context, learned workbenchservice.LearnedBlock) error {
		if !learned.Complete || learned.Reason != record.Reason || len(learned.Blockers) != 1 || len(learned.Digest) != 64 {
			t.Fatal(learned)
		}
		competing, err := acquireClaimLock(filepath.Join(directory, "claims.lock"), "test-competing", 20*time.Millisecond, time.Now())
		if err == nil {
			if competing != nil {
				_ = competing.Release()
			}
			t.Fatal("claims lock released before callback joined")
		}
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(raw) {
		t.Fatal("resolution changed host learned record", err)
	}
}
func TestResolutionLearnedTargetDoesNotIgnoreLegacyOrNativeCase(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "Acme", Name: "Issues"}
	native := repo
	native.Owner = "acme"
	native.Name = "issues"
	exact := blockedRecord{Repository: native, ItemID: "42", Reason: "answer needed"}
	records := map[string]blockedRecord{"exact": exact, "unrelated": {Repository: repo, ItemID: "77", Reason: "private other reason"}}
	one, err := resolutionLearnedTarget(records, repo, "42")
	if err != nil || !one.Complete || one.Reason != exact.Reason {
		t.Fatal(one, err)
	}
	changed := records["unrelated"]
	changed.Reason = "different unrelated reason"
	records["unrelated"] = changed
	two, err := resolutionLearnedTarget(records, repo, "42")
	if err != nil || two.Digest != one.Digest {
		t.Fatal("unrelated record changed exact evidence", two, err)
	}
	records["legacy"] = blockedRecord{ItemID: "42", Reason: "unscoped must not be guessed"}
	legacy, err := resolutionLearnedTarget(records, repo, "42")
	if err != nil || legacy.Complete {
		t.Fatal(legacy, err)
	}
	delete(records, "legacy")
	oversized := exact
	oversized.Reason = strings.Repeat("x", 8193)
	records["exact"] = oversized
	bounded, err := resolutionLearnedTarget(records, repo, "42")
	if err != nil || bounded.Complete || bounded.Reason != "" {
		t.Fatal(bounded, err)
	}
}
func TestResolutionLearnedScopeHonorsCanceledContextBeforeCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err := workbenchLearnedBlocks(t.TempDir())(ctx, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "issues"}, "42", func(context.Context, workbenchservice.LearnedBlock) error { called = true; return nil })
	if err == nil || called {
		t.Fatal(err, called)
	}
}
