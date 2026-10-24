package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

// TestRunJournalSeamIsComposedOnTheControllerAfterItsInputs: the seam has a
// constructor (so the assembly can complete), runs on the controller role
// (journal.* and control.claim are controller-only), and comes after every
// seam it reads from.
func TestRunJournalSeamIsComposedOnTheControllerAfterItsInputs(t *testing.T) {
	index := func(field string) int {
		return slices.IndexFunc(controlAssembly, func(seam controlAssemblySeam) bool { return seam.field == field })
	}
	at := index("RunJournal")
	if at < 0 {
		t.Fatal("RunJournal is not in the control assembly")
	}
	seam := controlAssembly[at]
	if seam.construct == nil || !seam.controller {
		t.Fatalf("RunJournal seam: constructed=%v controller=%v", seam.construct != nil, seam.controller)
	}
	for _, input := range []string{"Configuration", "Claims", "Journal", "Reader"} {
		if i := index(input); i < 0 || i > at {
			t.Errorf("%s must be composed before RunJournal (at %d, RunJournal at %d)", input, i, at)
		}
	}
}

type stubReaderBackend struct{ journal.ReaderBackend }

func TestConstructRunJournalRequiresEveryInput(t *testing.T) {
	complete := func() controlPlanes {
		return controlPlanes{
			journalReader: stubReaderBackend{},
			claimedItems:  func(context.Context, string) ([]string, error) { return nil, nil },
			Reader:        struct{ readservice.Reader }{},
			appliedSet:    func() (*instance.ConfigSet, bool) { return nil, false },
		}
	}
	for name, drop := range map[string]func(*controlPlanes){
		"journal reader":        func(p *controlPlanes) { p.journalReader = nil },
		"claims plane":          func(p *controlPlanes) { p.claimedItems = nil },
		"cloud reader":          func(p *controlPlanes) { p.Reader = nil },
		"applied configuration": func(p *controlPlanes) { p.appliedSet = nil },
	} {
		planes := complete()
		drop(&planes)
		if err := constructRunJournal(t.Context(), databaseServices{}, controlAuth{}, nil, nil, &planes); err == nil || planes.RunJournal != nil {
			t.Errorf("without the %s: err=%v, constructed=%v", name, err, planes.RunJournal != nil)
		}
	}
	planes := complete()
	if err := constructRunJournal(t.Context(), databaseServices{}, controlAuth{}, nil, nil, &planes); err != nil || planes.RunJournal == nil {
		t.Fatalf("with every input: err=%v, constructed=%v", err, planes.RunJournal != nil)
	}
}

func TestAppliedWorkflowsFilterByGaggleAndCapability(t *testing.T) {
	set := &instance.ConfigSet{Workflows: []apiv1.Workflow{
		{Spec: apiv1.WorkflowSpec{Gaggle: "web"}},
		{Spec: apiv1.WorkflowSpec{Gaggle: "api"}},
		{Spec: apiv1.WorkflowSpec{Gaggle: "web", DisplayName: "excluded"}},
	}}
	set.Workflows[0].Name, set.Workflows[1].Name, set.Workflows[2].Name = "zeta", "other", "alpha"
	applied := func() (*instance.ConfigSet, bool) { return set, true }
	can := func(spec apiv1.WorkflowSpec) bool { return spec.DisplayName != "excluded" }
	names, err := appliedWorkflows(applied, "web", can)
	if err != nil || !slices.Equal(names, []string{"zeta"}) {
		t.Fatalf("web workflows = %v, %v; want [zeta]", names, err)
	}
	all, err := appliedWorkflows(applied, "web", func(apiv1.WorkflowSpec) bool { return true })
	if err != nil || !slices.Equal(all, []string{"alpha", "zeta"}) {
		t.Fatalf("web workflows, any capability = %v, %v; want sorted [alpha zeta]", all, err)
	}
	if _, err := appliedWorkflows(func() (*instance.ConfigSet, bool) { return nil, false }, "web", can); err == nil || !strings.Contains(err.Error(), "no configuration generation is applied") {
		t.Fatalf("before any apply: %v, want a refusal", err)
	}
}
