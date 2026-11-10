package terminalwriter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

type sharedBackends struct{ caps []string }

func (b *sharedBackends) For(_ context.Context, _ Binding, _ providers.RepositoryRef, capability string) (Backend, error) {
	b.caps = append(b.caps, capability)
	return githubBackend(providers.NewGitHubProvider("")), nil
}

func TestSharedWriterScopesCapabilitiesAndRejectsHostMismatch(t *testing.T) {
	backends := &sharedBackends{}
	w, err := New(fakeRuns{"run": binding("run")}, backends, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.SharedClaimStore(t.Context(), "run", workspaceRepo); err != nil {
		t.Fatal(err)
	}
	if _, err := w.SharedClaimVisibility(t.Context(), "run", workspaceRepo); err != nil {
		t.Fatal(err)
	}
	if len(backends.caps) != 2 || backends.caps[0] != string(capability.RepoPush) || backends.caps[1] != string(capability.GitHubIssuesWrite) {
		t.Fatalf("capabilities=%v", backends.caps)
	}
	wrong := workspaceRepo
	wrong.URL = "https://different.example/api/v3"
	if _, err := w.SharedClaimStore(t.Context(), "run", wrong); !errors.Is(err, providerwriter.ErrRepositoryNotPinned) || len(backends.caps) != 2 {
		t.Fatalf("host mismatch=%v capabilities=%v", err, backends.caps)
	}
}

type failingSharedStore struct{ err error }

func (s failingSharedStore) Read(context.Context, string) (sharedclaim.Observation, error) {
	return sharedclaim.Observation{}, s.err
}
func (s failingSharedStore) CompareAndSwap(context.Context, string, string, sharedclaim.Record) error {
	return s.err
}

func TestSharedAdapterScrubsAndPreservesCASIdentities(t *testing.T) {
	scrubber := journal.NewRegistryScrubber()
	scrubber.Register([]byte("sensitive-provider-token"))
	for _, kind := range []error{sharedclaim.ErrConflict, sharedclaim.ErrHeld, sharedclaim.ErrNotOwner, context.Canceled} {
		store := scrubbedSharedStore{store: failingSharedStore{errors.Join(kind, errors.New("sensitive-provider-token"))}, scrubber: scrubber}
		err := store.CompareAndSwap(t.Context(), "42", "old", sharedclaim.Record{})
		if !errors.Is(err, kind) || strings.Contains(err.Error(), "sensitive-provider-token") {
			t.Fatalf("scrubbed protocol error=%v", err)
		}
	}
}

type countingLabels struct{ writes int }

func (*countingLabels) ReadClaimed(context.Context, string) (bool, error) { return false, nil }
func (l *countingLabels) SetClaimed(context.Context, string, bool) error  { l.writes++; return nil }

func TestSharedSweepRefusesLabelWriteWhenEpochJournalFails(t *testing.T) {
	labels := &countingLabels{}
	fenced := errors.New("epoch lost")
	visibility := scrubbedSharedVisibility{labels: labels, scrubber: journal.NewRegistryScrubber(), before: func(string, bool) error { return fenced }}
	if err := visibility.SetClaimed(t.Context(), "42", true); !errors.Is(err, fenced) || labels.writes != 0 {
		t.Fatalf("write after fence=%v, count=%d", err, labels.writes)
	}
}
