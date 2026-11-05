package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/worktree"
)

func TestWorkerRecoveryRefusesUnownedOrExpiredClaimBeforeCapture(t *testing.T) {
	for _, mode := range []string{"absent", "foreign", "expired", "ambiguous", "wrong-gaggle"} {
		t.Run(mode, func(t *testing.T) {
			claim := localscheduler.ClaimEntry{RunID: "source-run", Gaggle: "web", ItemID: "42", ExpiresAt: time.Now().Add(time.Hour)}
			entries := []localscheduler.ClaimEntry{claim}
			switch mode {
			case "absent":
				entries = nil
			case "foreign":
				entries[0].RunID = "other-run"
			case "expired":
				entries[0].ExpiresAt = time.Now().Add(-time.Hour)
			case "ambiguous":
				entries = append(entries, claim)
			case "wrong-gaggle":
				entries[0].Gaggle = "other"
			}
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/v1/claims/list" {
					t.Error("unexpected recovery upload before ownership validation")
				}
				if request.Header.Get("Authorization") != "Bearer worker-token" {
					t.Error("missing existing run credential")
				}
				_ = json.NewEncoder(response).Encode(map[string]any{"entries": entries})
			}))
			defer server.Close()
			root := t.TempDir()
			source := t.TempDir()
			file := filepath.Join(source, "implementation")
			if err := os.WriteFile(file, []byte("preserve me"), 0600); err != nil {
				t.Fatal(err)
			}
			worker := &workerSeams{root: root, recoveryEmitter: &livejournal.HTTPEmitter{BaseURL: server.URL, Token: "worker-token"}}
			manager := &worktree.Manager{Root: source}
			err := worker.publishWorkerRecovery(t.Context(), manager, worktree.CleanupTarget{
				Path: source, OwnerRunID: "source-run", Gaggle: "web", CreatedAt: time.Now(),
			})
			if err == nil {
				t.Fatal("invalid ownership permitted cleanup")
			}
			if data, err := os.ReadFile(file); err != nil || string(data) != "preserve me" {
				t.Fatalf("source changed before custody: %q %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(root, "recovery")); !os.IsNotExist(err) {
				t.Fatalf("capture preceded claim verification: %v", err)
			}
		})
	}
}
