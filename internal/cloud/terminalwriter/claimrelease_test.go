package terminalwriter

import (
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/cloud/claims"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestReleaseClaimUsesThePinnedBacklogAndExactRun(t *testing.T) {
	backend := &fakeBackend{}
	w, _ := newTestWriter(t, backend, fakeRuns{"run-1": binding("run-1")})
	entry := localscheduler.ClaimEntry{RunID: "run-1", Gaggle: "g", Provider: "github", ItemID: "7"}
	reason := claimrelease.Abandoned.Because("backlog_query_failed", "")
	if err := w.ReleaseClaim(t.Context(), entry, reason); err != nil {
		t.Fatal(err)
	}
	got := backend.released
	if got == nil || got.Repository != backlogRepo || got.ID != "7" || got.RunID != entry.RunID || got.ExpectedClaimRunID != entry.RunID || got.Reason != reason {
		t.Fatalf("provider request = %+v", got)
	}
}

func TestReleaseClaimRefusesIdentityMismatchBeforeProviderAccess(t *testing.T) {
	for _, field := range []string{"run", "gaggle", "provider"} {
		t.Run(field, func(t *testing.T) {
			backend := &fakeBackend{}
			w, _ := newTestWriter(t, backend, fakeRuns{"run-1": binding("run-1")})
			entry := localscheduler.ClaimEntry{RunID: "run-1", Gaggle: "g", Provider: "github", ItemID: "7"}
			switch field {
			case "run":
				entry.RunID = "run-unpinned"
			case "gaggle":
				entry.Gaggle = "other"
			case "provider":
				entry.Provider = "gitea"
			}
			if err := w.ReleaseClaim(t.Context(), entry, claimrelease.Reason{}); !errors.Is(err, claims.ErrProviderCleanupRefused) {
				t.Fatalf("mismatch = %v", err)
			}
			if len(*backend.calls) != 0 {
				t.Fatalf("provider called: %+v", *backend.calls)
			}
		})
	}
}

func TestReleaseClaimClassifiesAndScrubsFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
	}{
		{"successor", &providers.ClaimHeldError{Holder: "run-2", Expected: "run-1"}, true},
		{"transient", errors.New("provider unavailable ghs_secretvalue"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakeBackend{err: tc.err}
			w, registry := newTestWriter(t, backend, fakeRuns{"run-1": binding("run-1")})
			registry.Register([]byte("ghs_secretvalue"))
			err := w.ReleaseClaim(t.Context(), localscheduler.ClaimEntry{RunID: "run-1", Gaggle: "g", Provider: "github", ItemID: "7"}, claimrelease.Reason{})
			if err == nil || errors.Is(err, claims.ErrProviderCleanupRefused) != tc.permanent || strings.Contains(err.Error(), "ghs_secretvalue") {
				t.Fatalf("release = %v", err)
			}
		})
	}
}
