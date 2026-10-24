package terminalwriter

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/internal/triggerqueue"
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

func untouchedChildren(t *testing.T) ChildSource {
	return func(context.Context, string) (ChildLink, error) {
		t.Fatal("read child custody for a run with its own pin")
		return ChildLink{}, errors.New("unreachable")
	}
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
			Children:    untouchedChildren(t),
			Journal:     untouchedJournal{t},
			Generations: untouchedGenerations{t},
		}
		_, err := runs.Bind(t.Context(), "run-1")
		if !errors.Is(err, providerwriter.ErrRepositoryNotPinned) || !providerwriter.Permanent(err) {
			t.Errorf("%s pin: %v, want a permanent ErrRepositoryNotPinned", name, err)
		}
	}
}

type identityReader struct {
	journal.RunReader
	identity journal.RunIdentity
	err      error
}

func (r identityReader) Identity() (journal.RunIdentity, error) { return r.identity, r.err }

type identityJournal struct {
	identity journal.RunIdentity
	err      error
}

func (j identityJournal) OpenRun(context.Context, journal.RunRef) (journal.RunReader, error) {
	if j.err != nil {
		return nil, j.err
	}
	return identityReader{identity: j.identity}, nil
}

type oneGeneration struct{ set *instance.ConfigSet }

func (g oneGeneration) Acquire(context.Context, string) (childworkflow.Generation, error) {
	return childworkflow.Generation{Set: g.set, Release: func() error { return nil }}, nil
}

func childBindingFixture() (JournalRuns, ChildLink, journal.RunIdentity) {
	service := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "service"}
	link := ChildLink{
		Gaggle: "core", ParentRunID: "parent-run", StageOccurrence: "build/visit-1",
		InvocationKey: "child-1", AcceptanceID: "trigger-child-run",
	}
	identity := journal.RunIdentity{
		RunID: "child-run", Gaggle: "core", ConfigGeneration: "gen-1",
		Child: &journal.ChildLineage{
			Gaggle: link.Gaggle, ParentRunID: link.ParentRunID,
			StageOccurrence: link.StageOccurrence, InvocationKey: link.InvocationKey,
			AcceptanceID: link.AcceptanceID,
		},
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{
		ObjectMeta: metav1.ObjectMeta{Name: "core"},
		Spec: apiv1.GaggleSpec{
			Project: service,
			Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "acme/service"},
		},
	}}}
	runs := JournalRuns{
		Pins: func(_ context.Context, runID string) (RunPin, error) {
			if runID == link.ParentRunID {
				return RunPin{Gaggle: "core", ConfigGeneration: "gen-1", RepoRef: service}, nil
			}
			return RunPin{}, sql.ErrNoRows
		},
		Children:    func(context.Context, string) (ChildLink, error) { return link, nil },
		Journal:     identityJournal{identity: identity},
		Generations: oneGeneration{set: set},
	}
	return runs, link, identity
}

func TestBindChildInheritsItsParentsPin(t *testing.T) {
	runs, _, _ := childBindingFixture()
	bound, err := runs.Bind(t.Context(), "child-run")
	if err != nil {
		t.Fatal(err)
	}
	if bound.RunID != "child-run" || bound.Gaggle != "core" || bound.Generation != "gen-1" ||
		bound.Project.Owner != "acme" || bound.Project.Name != "service" ||
		!sameRepository(bound.Workspace, repositoryRef(bound.Project)) {
		t.Fatalf("binding = %+v", bound)
	}
}

func TestBindChildFallbackRefusals(t *testing.T) {
	tests := map[string]func(*JournalRuns, *ChildLink, *journal.RunIdentity){
		"unpinned non-child": func(r *JournalRuns, _ *ChildLink, _ *journal.RunIdentity) {
			r.Children = func(context.Context, string) (ChildLink, error) { return ChildLink{}, sql.ErrNoRows }
		},
		"invalid custody": func(_ *JournalRuns, link *ChildLink, _ *journal.RunIdentity) {
			link.ParentRunID = "child-run"
		},
		"parent has no pin": func(r *JournalRuns, _ *ChildLink, _ *journal.RunIdentity) {
			r.Pins = func(context.Context, string) (RunPin, error) { return RunPin{}, sql.ErrNoRows }
		},
		"parent gaggle mismatch": func(r *JournalRuns, _ *ChildLink, _ *journal.RunIdentity) {
			parentPin := r.Pins
			r.Pins = func(ctx context.Context, runID string) (RunPin, error) {
				pin, err := parentPin(ctx, runID)
				if err == nil {
					pin.Gaggle = "other"
				}
				return pin, err
			}
		},
		"journal has no child lineage": func(_ *JournalRuns, _ *ChildLink, id *journal.RunIdentity) {
			id.Child = nil
		},
		"journal parent mismatch": func(_ *JournalRuns, _ *ChildLink, id *journal.RunIdentity) {
			id.Child.ParentRunID = "other-parent"
		},
		"journal occurrence mismatch": func(_ *JournalRuns, _ *ChildLink, id *journal.RunIdentity) {
			id.Child.StageOccurrence = "other/visit"
		},
		"journal invocation mismatch": func(_ *JournalRuns, _ *ChildLink, id *journal.RunIdentity) {
			id.Child.InvocationKey = "other-child"
		},
		"journal acceptance mismatch": func(_ *JournalRuns, _ *ChildLink, id *journal.RunIdentity) {
			id.Child.AcceptanceID = "trigger-other-child"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			runs, link, identity := childBindingFixture()
			runs.Children = func(context.Context, string) (ChildLink, error) { return link, nil }
			runs.Journal = identityJournal{identity: identity}
			mutate(&runs, &link, &identity)
			runs.Children = func(context.Context, string) (ChildLink, error) { return link, nil }
			runs.Journal = identityJournal{identity: identity}
			if name == "unpinned non-child" {
				runs.Children = func(context.Context, string) (ChildLink, error) { return ChildLink{}, sql.ErrNoRows }
			}
			_, err := runs.Bind(t.Context(), "child-run")
			if !errors.Is(err, providerwriter.ErrRepositoryNotPinned) || !providerwriter.Permanent(err) {
				t.Fatalf("%v, want a permanent ErrRepositoryNotPinned", err)
			}
		})
	}
}

func TestBindChildControlGuardsRefuseBeforeJournalOrGeneration(t *testing.T) {
	for name, mutate := range map[string]func(*JournalRuns, *ChildLink){
		"lineage gaggle": func(_ *JournalRuns, link *ChildLink) {
			link.Gaggle = ""
		},
		"custody acceptance ID": func(_ *JournalRuns, link *ChildLink) {
			link.AcceptanceID = "trigger-another-run"
		},
		"parent custody gaggle": func(r *JournalRuns, _ *ChildLink) {
			parentPin := r.Pins
			r.Pins = func(ctx context.Context, runID string) (RunPin, error) {
				pin, err := parentPin(ctx, runID)
				if err == nil {
					pin.Gaggle = "other"
				}
				return pin, err
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			runs, link, _ := childBindingFixture()
			mutate(&runs, &link)
			runs.Children = func(context.Context, string) (ChildLink, error) { return link, nil }
			runs.Journal = untouchedJournal{t}
			runs.Generations = untouchedGenerations{t}
			_, err := runs.Bind(t.Context(), "child-run")
			if !errors.Is(err, providerwriter.ErrRepositoryNotPinned) || !providerwriter.Permanent(err) {
				t.Fatalf("%v, want a permanent ErrRepositoryNotPinned", err)
			}
		})
	}
}

func TestBindChildRefusesTombstonedAcceptanceCustody(t *testing.T) {
	runs, link, _ := childBindingFixture()
	runs.Children = AcceptanceChildren(func(context.Context, string) (triggerqueue.ChildRecord, error) {
		return triggerqueue.ChildRecord{
			Identity: triggerqueue.ChildIdentity{
				ChildParent:     triggerqueue.ChildParent{Gaggle: link.Gaggle, ParentRunID: link.ParentRunID},
				StageOccurrence: link.StageOccurrence,
				InvocationKey:   link.InvocationKey,
			},
			AcceptanceID: link.AcceptanceID,
			TombstonedAt: time.Now(),
		}, nil
	})
	runs.Journal = untouchedJournal{t}
	runs.Generations = untouchedGenerations{t}
	_, err := runs.Bind(t.Context(), "child-run")
	if !errors.Is(err, providerwriter.ErrRepositoryNotPinned) || !providerwriter.Permanent(err) {
		t.Fatalf("%v, want a permanent ErrRepositoryNotPinned", err)
	}
}

func TestBindChildFallbackReadFailuresAreRetryable(t *testing.T) {
	for name, fail := range map[string]func(*JournalRuns){
		"child custody": func(r *JournalRuns) {
			r.Children = func(context.Context, string) (ChildLink, error) {
				return ChildLink{}, errors.New("connection reset")
			}
		},
		"parent pin": func(r *JournalRuns) {
			r.Pins = func(_ context.Context, runID string) (RunPin, error) {
				if runID == "child-run" {
					return RunPin{}, sql.ErrNoRows
				}
				return RunPin{}, errors.New("connection reset")
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			runs, _, _ := childBindingFixture()
			fail(&runs)
			if _, err := runs.Bind(t.Context(), "child-run"); err == nil || providerwriter.Permanent(err) {
				t.Fatalf("%v, want a retryable error", err)
			}
		})
	}
}
