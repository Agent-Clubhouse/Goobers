package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

type fakeRepoWorkItemLister struct {
	items    []providers.WorkItem
	err      error
	requests []providers.ListWorkItemsRequest
}

func (f *fakeRepoWorkItemLister) ListWorkItems(
	_ context.Context,
	request providers.ListWorkItemsRequest,
) ([]providers.WorkItem, error) {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return nil, f.err
	}
	return append([]providers.WorkItem(nil), f.items...), nil
}

func testRepositoryRef() providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
}

// TestCheckRepoSelectorRealityCountsConjunction pins the semantics
// backlog-query itself applies: an item is eligible only when it carries EVERY
// selector label, so a repository full of items carrying one of them still
// matches nothing.
func TestCheckRepoSelectorRealityCountsConjunction(t *testing.T) {
	lister := &fakeRepoWorkItemLister{items: []providers.WorkItem{
		{ID: "1", Labels: []string{"bug", "reporting"}},
		{ID: "2", Labels: []string{"goobers:approved"}},
		{ID: "3", Labels: []string{"goobers:ready"}},
		{ID: "4", Labels: []string{"goobers:approved", "goobers:ready", "bug"}},
	}}
	reality, err := checkRepoSelectorReality(context.Background(), lister, testRepositoryRef(),
		[]string{"goobers:ready", "goobers:approved", " goobers:ready "})
	if err != nil {
		t.Fatal(err)
	}
	if reality.Open != 4 || reality.Matching != 1 || reality.Sampled {
		t.Fatalf("reality = %+v", reality)
	}
	if !slices.Equal(reality.Selectors, []string{"goobers:approved", "goobers:ready"}) {
		t.Fatalf("selectors = %v (want trimmed, de-duplicated, sorted)", reality.Selectors)
	}
	if reality.Mismatch() {
		t.Fatal("one eligible item must not read as a mismatch")
	}
	if len(lister.requests) != 1 ||
		lister.requests[0].State != "open" ||
		lister.requests[0].Limit != repoSelectorRealitySample ||
		lister.requests[0].Repository != testRepositoryRef() {
		t.Fatalf("requests = %+v", lister.requests)
	}
}

// TestCheckRepoSelectorRealityMismatchMessage is the cold-start ado #5 shape:
// five open work items, none carrying the required labels.
func TestCheckRepoSelectorRealityMismatchMessage(t *testing.T) {
	lister := &fakeRepoWorkItemLister{}
	for _, labels := range [][]string{
		{"bug", "reporting"}, {"cli", "enhancement"}, {"docs"}, {"bug"}, {},
	} {
		lister.items = append(lister.items, providers.WorkItem{Labels: labels})
	}
	reality, err := checkRepoSelectorReality(context.Background(), lister, testRepositoryRef(),
		[]string{"goobers:approved", "goobers"})
	if err != nil {
		t.Fatal(err)
	}
	if !reality.Mismatch() {
		t.Fatalf("reality = %+v, want a mismatch", reality)
	}
	summary := reality.Summary("acme/web")
	if summary != "backlog selectors (goobers, goobers:approved) match 0 of 5 open issues in acme/web" {
		t.Fatalf("summary = %q", summary)
	}
	if !strings.Contains(reality.Remedy(), "connect --seed") ||
		!strings.Contains(reality.Remedy(), "claim nothing") {
		t.Fatalf("remedy = %q", reality.Remedy())
	}
}

// TestCheckRepoSelectorRealitySingularAndSampled keeps the sentence honest at
// both ends: one issue is not "1 open issues", and a full page is reported as
// a sample rather than as the repository's true count.
func TestCheckRepoSelectorRealitySingularAndSampled(t *testing.T) {
	single := &fakeRepoWorkItemLister{items: []providers.WorkItem{{Labels: []string{"bug"}}}}
	reality, err := checkRepoSelectorReality(context.Background(), single, testRepositoryRef(), []string{"goobers:ready"})
	if err != nil {
		t.Fatal(err)
	}
	if got := reality.Summary("acme/web"); !strings.Contains(got, "0 of 1 open issue in acme/web") {
		t.Fatalf("summary = %q", got)
	}

	full := &fakeRepoWorkItemLister{}
	for range repoSelectorRealitySample {
		full.items = append(full.items, providers.WorkItem{Labels: []string{"bug"}})
	}
	sampled, err := checkRepoSelectorReality(context.Background(), full, testRepositoryRef(), []string{"goobers:ready"})
	if err != nil {
		t.Fatal(err)
	}
	if !sampled.Sampled {
		t.Fatalf("reality = %+v, want Sampled", sampled)
	}
	if got := sampled.Summary("acme/web"); !strings.Contains(got, "the first 100 open issues") {
		t.Fatalf("summary = %q", got)
	}
}

// TestCheckRepoSelectorRealityNoSelectorsNeverReads is the negative case: a
// gaggle that requires no labels is eligible for everything, so there is
// nothing to compare and no provider call to make.
func TestCheckRepoSelectorRealityNoSelectorsNeverReads(t *testing.T) {
	lister := &fakeRepoWorkItemLister{err: errors.New("must not be called")}
	for _, selectors := range [][]string{nil, {}, {"", "  "}} {
		reality, err := checkRepoSelectorReality(context.Background(), lister, testRepositoryRef(), selectors)
		if err != nil {
			t.Fatalf("selectors %v: %v", selectors, err)
		}
		if reality.Mismatch() {
			t.Fatalf("selectors %v: reality = %+v", selectors, reality)
		}
	}
	if len(lister.requests) != 0 {
		t.Fatalf("requests = %+v, want none", lister.requests)
	}
}

// TestRepoSelectorLabelsResolution pins the shared selector derivation both
// connect and validate read: backlog labels plus each workflow's
// trustLabel/requireLabels, with the gaggle's requireLabels default applying
// only to a workflow that declares none of its own, and another gaggle's
// workflows ignored.
func TestRepoSelectorLabelsResolution(t *testing.T) {
	gaggle := apiv1.Gaggle{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-web"},
		Spec: apiv1.GaggleSpec{
			Project:       apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
			Backlog:       apiv1.BacklogRef{Project: "acme/web", Labels: []string{"goobers"}},
			RequireLabels: []string{"goobers:ready"},
		},
	}
	workflows := []apiv1.Workflow{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "inherits-default"},
			Spec: apiv1.WorkflowSpec{Gaggle: "acme-web", Tasks: []apiv1.Task{{
				Name:   "query-backlog",
				Inputs: map[string]string{"trustLabel": "goobers:approved"},
			}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "declares-its-own"},
			Spec: apiv1.WorkflowSpec{Gaggle: "acme-web", Tasks: []apiv1.Task{{
				Name:   "query-backlog",
				Inputs: map[string]string{"requireLabels": "tier:1, tier:2"},
			}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "other-gaggle"},
			Spec: apiv1.WorkflowSpec{Gaggle: "somewhere-else", Tasks: []apiv1.Task{{
				Name:   "query-backlog",
				Inputs: map[string]string{"trustLabel": "not-mine"},
			}}},
		},
	}
	want := []string{"goobers", "goobers:approved", "goobers:ready", "tier:1", "tier:2"}
	if got := repoSelectorLabels(gaggle, workflows); !slices.Equal(got, want) {
		t.Fatalf("repoSelectorLabels = %v, want %v", got, want)
	}

	// A gaggle with no selectors at all is eligible for everything, and says so
	// by returning nothing rather than an empty-string label.
	bare := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "bare"}}
	if got := repoSelectorLabels(bare, nil); len(got) != 0 {
		t.Fatalf("repoSelectorLabels(bare) = %v, want none", got)
	}
}

func TestCheckRepoSelectorRealityPropagatesProviderError(t *testing.T) {
	sentinel := errors.New("403 from the forge")
	lister := &fakeRepoWorkItemLister{err: sentinel}
	if _, err := checkRepoSelectorReality(context.Background(), lister, testRepositoryRef(),
		[]string{"goobers:ready"}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
}
