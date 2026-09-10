package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

func TestSharedVisibilityCredentialFailureDoesNotChangeLifecycle(t *testing.T) {
	layout, _ := newPinnedClaimResolverRun(t, "shared")
	store := &pinnedClaimTestStore{}
	attempts := 0
	resolver := pinnedSharedClaimResolver{layout: layout,
		store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) { return store, nil },
		visibility: func(context.Context, providers.RepositoryRef) (sharedclaim.Visibility, error) {
			attempts++
			return nil, errors.New("issue credential unavailable")
		},
	}
	file, err := claimsclient.NewFile(claimsclient.FileConfig{LedgerPath: filepath.Join(layout.SchedulerDir(), "claims.json"), Shared: resolver})
	if err != nil {
		t.Fatal(err)
	}
	key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
	for range 2 {
		if ok, _, err := file.ClaimScoped(t.Context(), key, "shared-run", "claim", time.Minute); err != nil || !ok {
			t.Fatalf("acquire/renew depended on label credential: %v %v", ok, err)
		}
	}
	if err := file.ReleaseScoped(t.Context(), key, "shared-run"); err != nil {
		t.Fatalf("release depended on label credential: %v", err)
	}
	if attempts != 3 || store.record.Owner != (sharedclaim.Owner{}) {
		t.Fatalf("lifecycle did not attempt independent visibility: %d %+v", attempts, store.record)
	}
	for _, key := range []string{"pr/42", "decomposition-target:42", "merge-lock/repo", "042", "0"} {
		if resolver.visibilityTransition(providers.RepositoryRef{}, store, key) != nil {
			t.Fatalf("synthetic/noncanonical key reached visibility: %q", key)
		}
	}
}

type sharedVisibilityLabels struct {
	present bool
	writes  int
	err     error
	onWrite func()
}

func (l *sharedVisibilityLabels) ReadClaimed(context.Context, string) (bool, error) {
	return l.present, nil
}
func (l *sharedVisibilityLabels) SetClaimed(_ context.Context, _ string, present bool) error {
	l.writes++
	if l.err != nil {
		return l.err
	}
	l.present = present
	if l.onWrite != nil {
		l.onWrite()
	}
	return nil
}

func TestSharedConfirmationDoesNotLetLabelsDecideOwnership(t *testing.T) {
	for _, scenario := range []string{"success", "label-failure", "successor-during-label", "expired", "revoked", "remote-expired"} {
		t.Run(scenario, func(t *testing.T) {
			layout, _ := newPinnedClaimResolverRun(t, "shared")
			store := &pinnedClaimTestStore{}
			resolver := pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) { return store, nil }}
			key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
			binding, err := resolver.Admission(t.Context(), key, "shared-run", "claim")
			if err != nil {
				t.Fatal(err)
			}
			if err := sharedclaim.Acquire(t.Context(), store, binding.RemoteKey, binding.Owner, time.Minute); err != nil {
				t.Fatal(err)
			}
			entry := claimsclient.Entry{Gaggle: key.Gaggle, Provider: key.Provider, ExternalID: key.ExternalID,
				RunID: "shared-run", Workflow: "claim", SharedOwner: binding.Owner, ExpiresAt: time.Now().Add(time.Minute), SharedDeadline: time.Now().Add(time.Minute)}
			labels := &sharedVisibilityLabels{}
			switch scenario {
			case "label-failure":
				labels.err = errors.New("issue API unavailable")
			case "successor-during-label":
				labels.onWrite = func() { store.record.Owner.Token = "successor"; store.revision = "next" }
			case "expired":
				entry.SharedDeadline = time.Now().Add(-time.Second)
			case "revoked":
				entry.SharedRevoked = true
			case "remote-expired":
				store.record.ExpiresAt = time.Now().Add(-time.Second)
			}
			beforeWrites := store.writes
			var stderr strings.Builder
			result, err := confirmSharedClaimVisibility(t.Context(), entry, resolver, labels, &stderr)
			want := scenario == "success" || scenario == "label-failure"
			if result.Claimed != want || (err == nil) != want {
				t.Fatalf("confirmation = %+v, %v", result, err)
			}
			if store.writes != beforeWrites {
				t.Fatal("confirmation mutated authoritative ownership")
			}
			if scenario == "label-failure" && !strings.Contains(stderr.String(), "needs reconciliation") {
				t.Fatal("label failure was hidden")
			}
			if (scenario == "expired" || scenario == "revoked" || scenario == "remote-expired") && labels.writes != 0 {
				t.Fatal("invalid admission reached label write")
			}
		})
	}
}
