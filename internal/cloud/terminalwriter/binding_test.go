package terminalwriter

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
)

type untouchedJournal struct{ t *testing.T }

func (j untouchedJournal) OpenRun(context.Context, journal.RunRef) (journal.RunReader, error) {
	j.t.Fatal("opened the journal for a pin that can never bind")
	return nil, errors.New("unreachable")
}

type untouchedGenerations struct{ t *testing.T }

func (g untouchedGenerations) Acquire(context.Context, string) (childworkflow.Generation, error) {
	g.t.Fatal("pinned a generation for a pin that can never bind")
	return childworkflow.Generation{}, errors.New("unreachable")
}

// TestBindRefusesAnIncompletePinBeforeReadingAnything: the start fence records
// a pin the started input left empty as empty; a writer bind needs the gaggle,
// the generation and the repository, and refuses permanently without one.
func TestBindRefusesAnIncompletePinBeforeReadingAnything(t *testing.T) {
	service := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "service"}
	for name, pin := range map[string]RunPin{
		"empty":              {},
		"no gaggle":          {ConfigGeneration: "gen-1", RepoRef: service},
		"no generation":      {Gaggle: "core", RepoRef: service},
		"no repository":      {Gaggle: "core", ConfigGeneration: "gen-1"},
		"no repository name": {Gaggle: "core", ConfigGeneration: "gen-1", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme"}},
	} {
		runs := JournalRuns{
			Pins:        func(context.Context, string) (RunPin, error) { return pin, nil },
			Journal:     untouchedJournal{t},
			Generations: untouchedGenerations{t},
		}
		_, err := runs.Bind(t.Context(), "run-1")
		if !errors.Is(err, providerwriter.ErrRepositoryNotPinned) || !providerwriter.Permanent(err) {
			t.Errorf("%s pin: %v, want a permanent ErrRepositoryNotPinned", name, err)
		}
	}
}
