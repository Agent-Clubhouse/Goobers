package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// repolabels.go holds the one selector-vs-repository reality check the CLI
// shares: given the labels a gaggle's backlog selectors require for an item to
// be eligible, how many of the repository's open work items actually carry
// them. A config whose selectors match nothing validates clean and schedules
// forever without claiming anything (cold-start ado #5, python #7), so both
// `goobers connect` (the connect-time echo) and `goobers validate
// --check-repos` report it. The check is reality-dependent — it depends on
// what someone labelled this morning — so callers report it as a note or a
// WARNING, never as a structural error.
//
// The API here is deliberately self-contained: one read-only provider
// interface, one call, and formatting helpers that take the repository display
// string from the caller. It never reads config, never writes, and never
// decides a severity.

// repoWorkItemLister is the read-only provider surface the reality check
// needs. Both providers.GitHubProvider and the onboarding seeder satisfy it.
type repoWorkItemLister interface {
	ListWorkItems(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
}

// repoSelectorLabels returns the labels an item must carry for one gaggle's
// backlog selectors to pick it up: the gaggle's spec.backlog.labels, plus
// every one of its workflows' trustLabel/requireLabels inputs — falling back
// to the gaggle's spec.requireLabels default for a workflow that declares no
// requireLabels of its own, exactly as the runtime resolves it. workflows may
// contain other gaggles' workflows; they are filtered by spec.gaggle.
//
// This is the single definition of "your selectors" the connect-time echo and
// validate's repository check both compare against, so the two can never
// disagree about what a repository is expected to contain.
func repoSelectorLabels(gaggle apiv1.Gaggle, workflows []apiv1.Workflow) []string {
	labels := append([]string(nil), gaggle.Spec.Backlog.Labels...)
	for _, flow := range workflows {
		if flow.Spec.Gaggle != gaggle.Name {
			continue
		}
		declaresRequire := false
		for _, task := range flow.Spec.Tasks {
			if trust := task.Inputs["trustLabel"]; trust != "" {
				labels = append(labels, trust)
			}
			if require, ok := task.Inputs["requireLabels"]; ok {
				declaresRequire = true
				labels = append(labels, splitLabelList(require)...)
			}
		}
		if !declaresRequire {
			labels = append(labels, gaggle.Spec.RequireLabels...)
		}
	}
	return normalizeRepoSelectors(labels)
}

// repoSelectorRealitySample bounds the single page of open work items the
// check reads. The question is "does anything match", not "how many exactly",
// so one bounded page keeps a connect/validate run to one API call.
const repoSelectorRealitySample = 100

// repoSelectorReality is the comparison between a gaggle's required selector
// labels and the repository's open work items.
type repoSelectorReality struct {
	// Selectors are the labels an item must carry (all of them) to be
	// eligible, sorted and de-duplicated.
	Selectors []string
	// Open is the number of open work items examined.
	Open int
	// Matching is how many of those carry every selector label.
	Matching int
	// Sampled reports that Open hit the sample ceiling, so Open is a floor
	// rather than the repository's true open count.
	Sampled bool
}

// checkRepoSelectorReality reads one bounded page of the repository's open
// work items and reports how many satisfy every selector label. It returns a
// zero-value reality (Mismatch() false) when no selectors are declared, since
// a gaggle that requires no labels is eligible for everything.
func checkRepoSelectorReality(
	ctx context.Context,
	lister repoWorkItemLister,
	repository providers.RepositoryRef,
	selectors []string,
) (repoSelectorReality, error) {
	normalized := normalizeRepoSelectors(selectors)
	if len(normalized) == 0 || lister == nil {
		return repoSelectorReality{}, nil
	}
	items, err := lister.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: repository,
		State:      "open",
		Limit:      repoSelectorRealitySample,
	})
	if err != nil {
		return repoSelectorReality{}, err
	}
	reality := repoSelectorReality{
		Selectors: normalized,
		Open:      len(items),
		Sampled:   len(items) >= repoSelectorRealitySample,
	}
	for _, item := range items {
		if repoItemMatchesSelectors(item, normalized) {
			reality.Matching++
		}
	}
	return reality, nil
}

// repoItemMatchesSelectors reports whether item carries every selector label —
// the same conjunction `goobers backlog-query` applies when it decides an item
// is claimable.
func repoItemMatchesSelectors(item providers.WorkItem, selectors []string) bool {
	for _, selector := range selectors {
		if !item.HasLabel(selector) {
			return false
		}
	}
	return len(selectors) > 0
}

// normalizeRepoSelectors trims, de-duplicates, and sorts selector labels so
// two callers deriving the same set report it identically.
func normalizeRepoSelectors(selectors []string) []string {
	seen := make(map[string]bool, len(selectors))
	normalized := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		selector = strings.TrimSpace(selector)
		if selector == "" || seen[selector] {
			continue
		}
		seen[selector] = true
		normalized = append(normalized, selector)
	}
	sort.Strings(normalized)
	return normalized
}

// Mismatch reports the finding worth telling an operator about: selectors are
// declared and not one open work item satisfies them, so every scheduled run
// claims nothing.
func (r repoSelectorReality) Mismatch() bool {
	return len(r.Selectors) > 0 && r.Matching == 0
}

// Summary states the comparison without prescribing a fix. repository is the
// display identity (e.g. "acme/web") the caller already prints.
func (r repoSelectorReality) Summary(repository string) string {
	scope := fmt.Sprintf("%d open issue", r.Open)
	if r.Open != 1 {
		scope += "s"
	}
	if r.Sampled {
		scope = fmt.Sprintf("the first %d open issues", r.Open)
	}
	return fmt.Sprintf(
		"backlog selectors (%s) match %d of %s in %s",
		strings.Join(r.Selectors, ", "), r.Matching, scope, repository,
	)
}

// Remedy names the two ways out, in the order an operator should try them.
func (r repoSelectorReality) Remedy() string {
	return "so every scheduled run would claim nothing; " +
		"run `goobers connect --seed` to file a starter issue carrying those labels, " +
		"or add them to the issues you want Goobers to pick up"
}
