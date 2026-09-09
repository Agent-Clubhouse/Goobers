package localscheduler

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

func TestOpenClaimLedgerRejectsPartialSharedOwnership(t *testing.T) {
	now := time.Now().UTC()
	key := ClaimKey{Gaggle: "g", Provider: "github", ExternalID: "42"}
	storageKey, err := key.storageKey()
	if err != nil {
		t.Fatal(err)
	}
	base := ClaimEntry{ItemID: "42", ExternalID: "42", Gaggle: "g", Provider: "github", RunID: "run", Workflow: "implement",
		ClaimedAt: now, ExpiresAt: now.Add(time.Minute), SharedDeadline: now.Add(time.Minute),
		SharedOwner: sharedclaim.Owner{Instance: "instance", Run: "run", Token: "token"}}
	for _, location := range []string{"active", "history"} {
		for _, tc := range []struct {
			name   string
			mutate func(*ClaimEntry)
		}{
			{"missing-deadline", func(e *ClaimEntry) { e.SharedDeadline = time.Time{} }},
			{"missing-owner", func(e *ClaimEntry) { e.SharedOwner = sharedclaim.Owner{} }},
			{"partial-owner", func(e *ClaimEntry) { e.SharedOwner.Token = "" }},
			{"wrong-run", func(e *ClaimEntry) { e.SharedOwner.Run = "other" }},
			{"wrong-item", func(e *ClaimEntry) { e.ExternalID = "43" }},
			{"wrong-display-item", func(e *ClaimEntry) { e.ItemID = "43" }},
			{"wrong-namespace", func(e *ClaimEntry) { e.Gaggle = "other" }},
			{"extended-local-expiry", func(e *ClaimEntry) { e.ExpiresAt = now.Add(time.Hour) }},
			{"shortened-local-expiry", func(e *ClaimEntry) { e.ExpiresAt = now }},
		} {
			t.Run(location+"/"+tc.name, func(t *testing.T) {
				entry := base
				tc.mutate(&entry)
				state := claimLedgerState{Schema: claimLedgerSchema}
				if location == "active" {
					state.Entries = map[string]ClaimEntry{storageKey: entry}
				} else {
					state.History = map[string]map[string]ClaimEntry{"run": {storageKey: entry}}
				}
				assertSharedLedgerRejectedUnchanged(t, state)
			})
		}
	}
	t.Run("history-run-key", func(t *testing.T) {
		assertSharedLedgerRejectedUnchanged(t, claimLedgerState{Schema: claimLedgerSchema,
			History: map[string]map[string]ClaimEntry{"other": {storageKey: base}}})
	})
}

func assertSharedLedgerRejectedUnchanged(t *testing.T, state claimLedgerState) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claims.json")
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenClaimLedger(path); err == nil {
		t.Fatal("malformed shared ownership admitted on restart")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatalf("failed open changed durable evidence: %v", err)
	}
}
