package providers

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// This file holds the pure namespacing and cleanup-selection rules of the live
// ADO write leg (ado_live_write_test.go, -tags=liveadowrite; design
// docs/design/ado-parity-dsl-2-0.md §8.2, ADO-N16). They live in an untagged
// file so ordinary CI runs their unit tests: the tagged leg itself only runs
// where the scratch repository is provisioned, and a rule that picks another
// run's objects for cleanup must be caught long before that.

const (
	// adoLiveBranchRoot is the ref namespace every live-leg branch lives under.
	// The janitor never considers a pull request whose source branch is
	// outside it.
	adoLiveBranchRoot = "goobers-live/"
	// adoLiveTag is the work-item tag and pull-request label the leg stamps on
	// everything it creates, so a human can find leftovers in the scratch
	// project at a glance.
	adoLiveTag = "goobers-live"
	// adoLiveJanitorAge is how old another run's pull request must be before
	// the janitor abandons it: long enough that a concurrent or re-run leg is
	// never swept mid-flight.
	adoLiveJanitorAge = 24 * time.Hour
	// adoLiveRunIDEnv overrides the run id; CI sets it from github.run_id so a
	// re-run of the same workflow run converges on the same objects.
	adoLiveRunIDEnv = "GOOBERS_ADO_LIVE_RUN_ID"
)

// adoLiveRunIDPattern keeps a run id safe as one git ref path segment and as a
// WIQL full-text word: no slash, no whitespace, no leading dot or dash.
var adoLiveRunIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// adoLiveNamespace scopes every object one live-leg run creates.
type adoLiveNamespace struct {
	runID string
}

// adoLiveRunID resolves the run id: the explicit override when set, otherwise
// a local, time-derived id so two developer runs never share a namespace.
func adoLiveRunID(getenv func(string) string, now time.Time) string {
	if id := strings.TrimSpace(getenv(adoLiveRunIDEnv)); id != "" {
		return id
	}
	return "local-" + now.UTC().Format("20060102t150405")
}

func newADOLiveNamespace(runID string) (adoLiveNamespace, error) {
	if !adoLiveRunIDPattern.MatchString(runID) || strings.Contains(runID, "..") {
		return adoLiveNamespace{}, fmt.Errorf("live run id %q must match %s and contain no \"..\"", runID, adoLiveRunIDPattern)
	}
	return adoLiveNamespace{runID: runID}, nil
}

// branchPrefix is the ref prefix owned by this run.
func (n adoLiveNamespace) branchPrefix() string {
	return adoLiveBranchRoot + n.runID + "/"
}

// branch names one scenario's source branch inside this run's namespace.
func (n adoLiveNamespace) branch(scenario string) string {
	return n.branchPrefix() + scenario
}

// ownsBranch reports whether name is one of this run's branches. Cleanup
// deletes a branch only when this holds.
func (n adoLiveNamespace) ownsBranch(name string) bool {
	name = strings.TrimPrefix(name, "refs/heads/")
	return strings.HasPrefix(name, n.branchPrefix()) && len(name) > len(n.branchPrefix())
}

// itemRunID is the provider RunID for one scenario's work item: its run-id
// footer is what CreateWorkItem's find-or-create keys on, so a re-run with the
// same run id converges on the same item.
func (n adoLiveNamespace) itemRunID(scenario string) string {
	return adoLiveTag + "-" + n.runID + "-" + scenario
}

// ownsItemBody reports whether a work-item body carries this run's footer for
// the scenario. Cleanup retires a work item only when this holds. The footer
// must end at a non-id character, so run "gh-12" never claims "gh-123"'s item
// and scenario "wi" never claims "wi2"'s. ADO stores descriptions as HTML, so
// the match is not anchored to a line of its own.
func (n adoLiveNamespace) ownsItemBody(body, scenario string) bool {
	footer := regexp.MustCompile(regexp.QuoteMeta(runFooter(n.itemRunID(scenario))) + `(?:$|[^A-Za-z0-9._-])`)
	return footer.MatchString(body)
}

// adoLiveOwnPullRequests selects this run's pull requests for cleanup. It
// never selects another run's pull request or one outside the live namespace.
func adoLiveOwnPullRequests(n adoLiveNamespace, prs []PullRequestSummary) []PullRequestSummary {
	var own []PullRequestSummary
	for _, pr := range prs {
		if n.ownsBranch(pr.Head) {
			own = append(own, pr)
		}
	}
	return own
}

// adoLiveJanitorPullRequests selects other runs' abandoned-in-place pull
// requests: a live-namespace source branch, not this run's, and created more
// than adoLiveJanitorAge before now. ADO's ListPullRequests carries the
// creation date in UpdatedAt; a zero time is never old enough.
func adoLiveJanitorPullRequests(n adoLiveNamespace, prs []PullRequestSummary, now time.Time) []PullRequestSummary {
	var stale []PullRequestSummary
	for _, pr := range prs {
		head := strings.TrimPrefix(pr.Head, "refs/heads/")
		if !strings.HasPrefix(head, adoLiveBranchRoot) || n.ownsBranch(head) {
			continue
		}
		if pr.UpdatedAt.IsZero() || now.Sub(pr.UpdatedAt) <= adoLiveJanitorAge {
			continue
		}
		stale = append(stale, pr)
	}
	return stale
}

// adoLiveRetireState picks the state a live work item is retired to: the
// type's Removed-category state, else its Completed-category state (§8.2:
// "Removed, or Closed where the type has no Removed").
func adoLiveRetireState(states []adoWorkItemState) (string, bool) {
	for _, category := range []string{"Removed", "Completed"} {
		for _, state := range states {
			if strings.EqualFold(state.Category, category) {
				return state.Name, true
			}
		}
	}
	return "", false
}

func TestADOLiveRunIDPrefersOverrideAndDefaultsToLocal(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 25, 8, 30, 5, 0, time.UTC)
	env := map[string]string{adoLiveRunIDEnv: " gh-123 "}
	if got := adoLiveRunID(func(k string) string { return env[k] }, now); got != "gh-123" {
		t.Fatalf("override run id = %q, want gh-123", got)
	}
	got := adoLiveRunID(func(string) string { return "" }, now)
	if got != "local-20260925t083005" {
		t.Fatalf("default run id = %q", got)
	}
	if _, err := newADOLiveNamespace(got); err != nil {
		t.Fatalf("default run id must be a valid namespace: %v", err)
	}
}

func TestADOLiveNamespaceRejectsUnsafeRunIDs(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", "a/b", "../x", "a..b", "-lead", ".lead", "has space", strings.Repeat("x", 65)} {
		if _, err := newADOLiveNamespace(id); err == nil {
			t.Errorf("newADOLiveNamespace(%q) accepted an unsafe run id", id)
		}
	}
	for _, id := range []string{"gh-123", "local-20260925t083005", "7"} {
		if _, err := newADOLiveNamespace(id); err != nil {
			t.Errorf("newADOLiveNamespace(%q): %v", id, err)
		}
	}
}

func TestADOLiveNamespaceOwnsOnlyItsOwnBranches(t *testing.T) {
	t.Parallel()
	ns, err := newADOLiveNamespace("gh-12")
	if err != nil {
		t.Fatal(err)
	}
	if got := ns.branch("pr"); got != "goobers-live/gh-12/pr" {
		t.Fatalf("branch = %q", got)
	}
	for name, want := range map[string]bool{
		"goobers-live/gh-12/pr":            true,
		"refs/heads/goobers-live/gh-12/pr": true,
		"goobers-live/gh-12/":              false, // the namespace root itself is not a branch
		"goobers-live/gh-123/pr":           false, // a run id sharing our prefix
		"goobers-live/gh-1/pr":             false,
		"goobers-live/other/pr":            false,
		"goobers/gh-12/pr":                 false,
		"main":                             false,
	} {
		if got := ns.ownsBranch(name); got != want {
			t.Errorf("ownsBranch(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestADOLiveNamespaceOwnsOnlyItsOwnItemFooter(t *testing.T) {
	t.Parallel()
	ns, _ := newADOLiveNamespace("gh-12")
	for _, own := range []string{
		withRunIDFooter("body", ns.itemRunID("wi")),
		"<div>body</div><div>---</div><div>" + runFooter(ns.itemRunID("wi")) + "</div>",
	} {
		if !ns.ownsItemBody(own, "wi") {
			t.Fatalf("own footer not recognized in %q", own)
		}
	}
	other, _ := newADOLiveNamespace("gh-123")
	for _, body := range []string{
		withRunIDFooter("body", other.itemRunID("wi")),
		withRunIDFooter("body", ns.itemRunID("wi2")),
		withRunIDFooter("body", "goobers-live-gh-12-wi-extra"),
		"no footer",
	} {
		if ns.ownsItemBody(body, "wi") {
			t.Errorf("ownsItemBody claimed a foreign body %q", body)
		}
	}
}

func TestADOLiveCleanupSelectsOnlyThisRunsPullRequests(t *testing.T) {
	t.Parallel()
	ns, _ := newADOLiveNamespace("gh-12")
	prs := []PullRequestSummary{
		{ID: "1", Head: "goobers-live/gh-12/pr"},
		{ID: "2", Head: "goobers-live/gh-123/pr"},
		{ID: "3", Head: "goobers/run-9"},
		{ID: "4", Head: "feature/goobers-live/gh-12/pr"},
		{ID: "5", Head: "goobers-live/gh-12/labels"},
	}
	got := adoLiveIDs(adoLiveOwnPullRequests(ns, prs))
	if got != "1,5" {
		t.Fatalf("cleanup selected %q, want 1,5", got)
	}
}

func TestADOLiveJanitorSelectsOnlyStaleForeignLivePullRequests(t *testing.T) {
	t.Parallel()
	ns, _ := newADOLiveNamespace("gh-12")
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	old := now.Add(-adoLiveJanitorAge - time.Minute)
	prs := []PullRequestSummary{
		{ID: "1", Head: "goobers-live/gh-9/pr", UpdatedAt: old},                         // stale, other run: abandon
		{ID: "2", Head: "goobers-live/gh-9/pr", UpdatedAt: now.Add(-time.Hour)},         // too young
		{ID: "3", Head: "goobers-live/gh-12/pr", UpdatedAt: old},                        // our own run
		{ID: "4", Head: "goobers/run-1", UpdatedAt: old},                                // not live-prefixed
		{ID: "5", Head: "goobers-live/gh-9/pr", UpdatedAt: time.Time{}},                 // unknown age
		{ID: "6", Head: "refs/heads/goobers-live/gh-8/pr", UpdatedAt: old},              // ref form
		{ID: "7", Head: "goobers-live/gh-9/pr", UpdatedAt: now.Add(-adoLiveJanitorAge)}, // exactly at the boundary
	}
	if got := adoLiveIDs(adoLiveJanitorPullRequests(ns, prs, now)); got != "1,6" {
		t.Fatalf("janitor selected %q, want 1,6", got)
	}
}

func TestADOLiveRetireStatePrefersRemovedThenCompleted(t *testing.T) {
	t.Parallel()
	withRemoved := []adoWorkItemState{{Name: "New", Category: "Proposed"}, {Name: "Closed", Category: "Completed"}, {Name: "Removed", Category: "Removed"}}
	if got, ok := adoLiveRetireState(withRemoved); !ok || got != "Removed" {
		t.Fatalf("retire state = %q, %v; want Removed", got, ok)
	}
	withoutRemoved := []adoWorkItemState{{Name: "To Do", Category: "Proposed"}, {Name: "Done", Category: "Completed"}}
	if got, ok := adoLiveRetireState(withoutRemoved); !ok || got != "Done" {
		t.Fatalf("retire state = %q, %v; want Done", got, ok)
	}
	if _, ok := adoLiveRetireState([]adoWorkItemState{{Name: "New", Category: "Proposed"}}); ok {
		t.Fatal("a type with no terminal state must not report a retire state")
	}
}

func adoLiveIDs(prs []PullRequestSummary) string {
	out := make([]string, 0, len(prs))
	for _, pr := range prs {
		out = append(out, pr.ID)
	}
	return strings.Join(out, ",")
}
