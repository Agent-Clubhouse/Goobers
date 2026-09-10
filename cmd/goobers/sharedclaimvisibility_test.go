package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

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
