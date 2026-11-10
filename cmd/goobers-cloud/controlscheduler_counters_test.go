package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/remediationselect"
)

type stubClaims struct{}

func (stubClaims) ListNamespace(context.Context, string, string) (claimsclient.Listing, error) {
	return claimsclient.Listing{}, nil
}

type stubJournal struct{}

func (stubJournal) BranchOwnership(context.Context, journalclient.BranchOwnershipRequest) (journalclient.BranchOwnershipResponse, error) {
	return journalclient.BranchOwnershipResponse{}, nil
}

type nopRegistrar struct{}

func (nopRegistrar) Register([]byte) {}

type fixedResolver struct{ refs *[]string }

func (r fixedResolver) Resolve(_ context.Context, name string) (string, error) {
	*r.refs = append(*r.refs, name)
	return "token-" + name, nil
}

func candidateWithConfig() cloudconfig.Candidate {
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "g"}, Spec: apiv1.GaggleSpec{
			Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "o", Name: "r", Branch: "main"},
		}}},
		Workflows: []apiv1.Workflow{{ObjectMeta: metav1.ObjectMeta{Name: "wf"}, Spec: apiv1.WorkflowSpec{Gaggle: "g"}}},
	}
	cfg := &instance.Config{Timezone: "UTC", SelfIdentity: "the-bot", Repos: []instance.RepoRef{{Provider: "github", Owner: "o", Name: "r"}}}
	return cloudconfig.Candidate{Digest: "gen-1", Config: cfg, Set: set}
}

func staticCredentials(refs *[]string) counterCredentials {
	return func(*instance.Config) (credentials.Resolver, error) { return fixedResolver{refs: refs}, nil }
}

// The production factory builds GitHub-backed counters over the SHARED quota
// state, with every gate enforced when the claims plane and the journal read are
// composed.
func TestProviderCountersEnforceEveryGateWhenFullyComposed(t *testing.T) {
	quota := localscheduler.NewProviderQuotaState()
	var refs []string
	factory := newProviderCounters(staticCredentials(&refs), stubClaims{}, stubJournal{}, nopRegistrar{}, func(error) {})
	counters, err := factory(candidateWithConfig(), quota)
	if err != nil {
		t.Fatal(err)
	}
	if counters == nil || counters.OpenPRs() == nil {
		t.Fatal("no counters, or no open-PR counter")
	}
	reporter, ok := counters.(gateReporter)
	if !ok || len(reporter.DisabledGates()) != 0 {
		t.Fatalf("a fully composed generation reports disabled gates: %v", reporter)
	}
}

// Without the claims plane or the journal read, the other counters still run but
// the schedule-demand gate is reported as not enforced.
func TestProviderCountersReportScheduleDemandUnenforcedWithoutItsInputs(t *testing.T) {
	quota := localscheduler.NewProviderQuotaState()
	var refs []string
	type variant struct {
		claims  remediationselect.NamespaceLister
		journal remediationselect.BranchOwnershipReader
	}
	for name, v := range map[string]variant{
		"no claims":  {nil, stubJournal{}},
		"no journal": {stubClaims{}, nil},
		"neither":    {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			factory := newProviderCounters(staticCredentials(&refs), v.claims, v.journal, nopRegistrar{}, func(error) {})
			counters, err := factory(candidateWithConfig(), quota)
			if err != nil {
				t.Fatal(err)
			}
			disabled := counters.(gateReporter).DisabledGates()
			if _, ok := disabled[scheduleDemandGate]; !ok || len(disabled) != 1 {
				t.Fatalf("disabled gates = %v, want exactly %s", disabled, scheduleDemandGate)
			}
		})
	}
}

func TestProviderCountersDegradeCleanlyWithoutAConfigOrCredentials(t *testing.T) {
	quota := localscheduler.NewProviderQuotaState()
	var refs []string
	factory := newProviderCounters(staticCredentials(&refs), stubClaims{}, stubJournal{}, nopRegistrar{}, func(error) {})
	if _, err := factory(cloudconfig.Candidate{Digest: "x"}, quota); !errors.Is(err, ErrCountersDisabled) {
		t.Fatalf("no applied config = %v, want ErrCountersDisabled", err)
	}
	broken := newProviderCounters(func(*instance.Config) (credentials.Resolver, error) { return nil, errors.New("secret store down") }, stubClaims{}, stubJournal{}, nopRegistrar{}, func(error) {})
	if _, err := broken(candidateWithConfig(), quota); err == nil || errors.Is(err, ErrCountersDisabled) || !strings.Contains(err.Error(), "secret store down") {
		t.Fatalf("a credentials failure must surface as a real error, got %v", err)
	}
	if _, err := factory(candidateWithConfig(), nil); err == nil {
		t.Fatal("counters were built without the shared quota state")
	}
}

type reportingCounters struct {
	fakeCounters
	disabled map[string]string
}

func (c *reportingCounters) DisabledGates() map[string]string { return c.disabled }

// Composed counters that leave a gate unenforced keep exactly that gate on the
// readiness checks and journal ONE warning per epoch for it (a generation change
// does not warn again); the other gates clear.
func TestAdmissionKeepsAPartiallyUnenforcedGateVisibleAndJournaledOnce(t *testing.T) {
	f := newAdmissionFixture(t, apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, false)
	f.comp.Counters = func(cloudconfig.Candidate, *localscheduler.ProviderQuotaState) (generationCounters, error) {
		return &reportingCounters{fakeCounters: fakeCounters{runCtx: make(chan context.Context, 1)}, disabled: map[string]string{scheduleDemandGate: "no claims plane"}}, nil
	}
	f.gates.Set("max-open-prs", "stale")
	epoch, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := f.build(epoch)
	if err := a.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	checks := f.gates.Checks()
	if len(checks) != 1 || checks["scheduler-gate:"+scheduleDemandGate] {
		t.Fatalf("checks = %v, want only the schedule-demand gate reported unenforced", checks)
	}
	f.setApplied("gen-2")
	if err := a.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, err := f.log.ReadAfterSeq(0)
	if err != nil {
		t.Fatal(err)
	}
	warnings := 0
	for _, e := range events {
		if e.Type == journal.EventRunnerAnnotation && e.Name == "scheduler.gate-not-enforced" {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("journaled gate warnings = %d, want 1 per epoch", warnings)
	}
	cancel()
	stopAdmission(t, a)
}
