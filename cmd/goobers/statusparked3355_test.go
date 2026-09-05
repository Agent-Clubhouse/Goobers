package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// stubStatusParkedBacklog points the status parked-item loader at a canned
// snapshot for tests that are not about the provider query itself.
func stubStatusParkedBacklog(
	t *testing.T,
	load func(context.Context, *instance.Config) (statusParkedBacklog, error),
) {
	t.Helper()
	previous := loadStatusParkedBacklog
	loadStatusParkedBacklog = load
	t.Cleanup(func() { loadStatusParkedBacklog = previous })
}

// useFakeStatusProvider routes both status provider queries at the fake
// GitHub server.
func useFakeStatusProvider(t *testing.T, server *fakeGitHubServer) {
	t.Helper()
	previousProvider := newStatusGitHubProvider
	newStatusGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newStatusGitHubProvider = previousProvider })
}

// TestStatusListsBacklogItemsParkedOutOfTheReadyPool reproduces #3355's
// observed shape: park-escalation via issue-close-out strips goobers:ready and
// adds goobers:needs-remediation, and query-backlog requires goobers:ready —
// so the item leaves the instance's backlog view with nothing to put it back.
// Status must show it.
func TestStatusListsBacklogItemsParkedOutOfTheReadyPool(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GITHUB_TOKEN", "status-fixture-token")

	server := newFakeGitHubServer(t, "your-org", "your-repo")
	// #168: the timestamped case from the issue — ready removed, parked
	// needs-remediation by park-escalated.
	server.addIssue(168, "seed: broken base", needsRemediationLabel)
	// #9 parked on a human decision; lower number proves numeric ordering.
	server.addIssue(9, "policy call pending", providers.LabelNeedsHuman)
	// #266: still ready, still selectable — not this section's subject.
	server.addIssue(266, "eligible work", providers.LabelReady)
	// #300 carries a park label AND ready: backlog reconcile resolves that
	// conflict, and until it does the item is still selectable, so status must
	// not report it as invisible.
	server.addIssue(300, "conflicted labels", providers.LabelReady, blockedOnSiblingLabel)

	useFakeStatusProvider(t, server)

	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	countLine := "Parked backlog items (park label, no goobers:ready — not selectable): 2"
	if !strings.Contains(stdout, countLine) {
		t.Fatalf("stdout = %q, want %q", stdout, countLine)
	}
	humanAt := strings.Index(stdout, "#9 goobers:needs-human — policy call pending")
	remediationAt := strings.Index(stdout, "#168 goobers:needs-remediation — seed: broken base")
	if humanAt == -1 || remediationAt == -1 || humanAt > remediationAt {
		t.Fatalf("stdout = %q, want both parked items listed in numeric order", stdout)
	}
	if !strings.Contains(stdout, "No workflow re-adds goobers:ready") {
		t.Fatalf("stdout = %q, want the no-automatic-re-entry note", stdout)
	}
	for _, unwanted := range []string{"#266", "#300"} {
		if strings.Contains(stdout, unwanted) {
			t.Fatalf("stdout = %q, want no ready-pool item %s in the parked section", stdout, unwanted)
		}
	}
}

// TestStatusParkedBacklogReportsEveryParkDisposition locks the disposition set
// to backlogreconcile.go's itemHasParkLabel: every label that cannot coexist
// with goobers:ready must be reported, including an item parked twice.
func TestStatusParkedBacklogReportsEveryParkDisposition(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GITHUB_TOKEN", "status-fixture-token")

	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(11, "human", providers.LabelNeedsHuman)
	server.addIssue(12, "sibling", blockedOnSiblingLabel)
	server.addIssue(13, "remediation", needsRemediationLabel)
	server.addIssue(14, "both", blockedOnSiblingLabel, needsRemediationLabel)
	server.setIssueState(13, "closed")

	useFakeStatusProvider(t, server)

	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	parked, err := queryStatusParkedBacklog(context.Background(), cfg)
	if err != nil {
		t.Fatalf("queryStatusParkedBacklog: %v", err)
	}
	// A closed item is off the backlog by decision, not by silent removal.
	if parked.Total != 3 || len(parked.Items) != 3 {
		t.Fatalf("parked = %+v, want three open parked items", parked)
	}
	wantRefs := []string{"#11", "#12", "#14"}
	for i, want := range wantRefs {
		if parked.Items[i].Ref != want {
			t.Fatalf("item %d ref = %q, want %q (parked = %+v)", i, parked.Items[i].Ref, want, parked)
		}
	}
	both := parked.Items[2]
	if len(both.Dispositions) != 2 ||
		both.Dispositions[0] != blockedOnSiblingLabel ||
		both.Dispositions[1] != needsRemediationLabel {
		t.Fatalf("dispositions = %v, want both park labels once each", both.Dispositions)
	}
}

// TestStatusReportsParkedBacklogWhenPullRequestCountsFail guards the wiring:
// the parked section is the one an unattended instance most needs, so a failed
// open-PR query must not swallow it.
func TestStatusReportsParkedBacklogWhenPullRequestCountsFail(t *testing.T) {
	root := initDemo(t)
	previousPRLoader := loadStatusPRLabelCounts
	loadStatusPRLabelCounts = func(context.Context, *instance.Config) (statusPRLabelCounts, error) {
		return statusPRLabelCounts{}, errors.New("provider unavailable")
	}
	t.Cleanup(func() { loadStatusPRLabelCounts = previousPRLoader })
	stubStatusParkedBacklog(t, func(context.Context, *instance.Config) (statusParkedBacklog, error) {
		return statusParkedBacklog{
			Total: 1,
			Items: []statusParkedItem{{
				ID:           "168",
				Ref:          "#168",
				Title:        "seed: broken base",
				Dispositions: []string{needsRemediationLabel},
			}},
		}, nil
	})

	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"Open PR label counts unavailable: provider unavailable",
		"Parked backlog items (park label, no goobers:ready — not selectable): 1",
		"#168 goobers:needs-remediation — seed: broken base",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	}
}

func TestStatusKeepsLocalOutputWhenParkedBacklogIsUnavailable(t *testing.T) {
	root := initDemo(t)
	stubStatusParkedBacklog(t, func(context.Context, *instance.Config) (statusParkedBacklog, error) {
		return statusParkedBacklog{}, errors.New("provider unavailable")
	})

	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"Parked backlog items unavailable: provider unavailable",
		"no runs found",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
	}
}

func TestStatusJSONCarriesParkedBacklog(t *testing.T) {
	root := initDemo(t)
	stubStatusParkedBacklog(t, func(context.Context, *instance.Config) (statusParkedBacklog, error) {
		return statusParkedBacklog{
			Total: 1,
			Items: []statusParkedItem{{
				ID:           "168",
				Ref:          "#168",
				Title:        "seed: broken base",
				Dispositions: []string{needsRemediationLabel},
			}},
		}, nil
	})

	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status --json: code = %d, stderr = %q", code, stderr)
	}
	var output statusJSONOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("decode status json %q: %v", stdout, err)
	}
	if output.ParkedBacklog == nil || output.ParkedBacklog.Total != 1 ||
		len(output.ParkedBacklog.Items) != 1 || output.ParkedBacklog.Items[0].Ref != "#168" {
		t.Fatalf("parkedBacklog = %+v, want the parked item snapshot", output.ParkedBacklog)
	}
}

func TestStatusParkedBacklogPagesPastTheQueryLimit(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GITHUB_TOKEN", "status-fixture-token")

	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for i := 1; i <= statusParkedBacklogQueryLimit+25; i++ {
		server.addIssue(i, fmt.Sprintf("parked %d", i), needsRemediationLabel)
	}
	useFakeStatusProvider(t, server)

	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	parked, err := queryStatusParkedBacklog(context.Background(), cfg)
	if err != nil {
		t.Fatalf("queryStatusParkedBacklog: %v", err)
	}
	if parked.Total < statusParkedBacklogQueryLimit {
		t.Fatalf("parked.Total = %d, want >= %d", parked.Total, statusParkedBacklogQueryLimit)
	}
	if len(parked.Items) != statusParkedBacklogListLimit {
		t.Fatalf("len(parked.Items) = %d, want %d (display cap preserved)", len(parked.Items), statusParkedBacklogListLimit)
	}
}

// TestStatusJSONOmitsParkedBacklogWhenUnavailable keeps the field additive: a
// consumer sees no key rather than a zero snapshot that would read as "nothing
// is parked" when the provider was simply unreachable.
func TestStatusJSONOmitsParkedBacklogWhenUnavailable(t *testing.T) {
	root := initDemo(t)
	stubStatusParkedBacklog(t, func(context.Context, *instance.Config) (statusParkedBacklog, error) {
		return statusParkedBacklog{}, errors.New("provider unavailable")
	})

	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 {
		t.Fatalf("status --json: code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, "parkedBacklog") {
		t.Fatalf("stdout = %q, want parkedBacklog omitted when unavailable", stdout)
	}
}

func TestParkedBacklogStatusTextTruncatesTheList(t *testing.T) {
	parked := statusParkedBacklog{Total: statusParkedBacklogListLimit + 3}
	for i := 0; i < statusParkedBacklogListLimit; i++ {
		parked.Items = append(parked.Items, statusParkedItem{
			ID:           "1",
			Ref:          "#1",
			Dispositions: []string{needsRemediationLabel},
		})
	}
	text := parkedBacklogStatusText(parked)
	if !strings.Contains(text, "... and 3 more") {
		t.Fatalf("text = %q, want a truncation note", text)
	}
	if strings.Contains(text, "#1 goobers:needs-remediation —") {
		t.Fatalf("text = %q, want no empty title separator", text)
	}
}

func TestParkedBacklogStatusTextReportsAnEmptyPool(t *testing.T) {
	text := parkedBacklogStatusText(statusParkedBacklog{})
	if text != "Parked backlog items (park label, no goobers:ready — not selectable): 0\n" {
		t.Fatalf("text = %q, want a bare zero count", text)
	}
}

func TestStatusParkedBacklogUsesBoundedRefreshCadence(t *testing.T) {
	now := time.Date(2026, time.August, 20, 5, 36, 15, 0, time.UTC)
	loads := 0
	cache := &statusParkedBacklogCache{
		load: func(context.Context, *instance.Config) (statusParkedBacklog, error) {
			loads++
			return statusParkedBacklog{Total: loads}, nil
		},
		now: func() time.Time { return now },
	}

	first, err := cache.Load(context.Background(), &instance.Config{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(localscheduler.DefaultOpenPRRefreshInterval - time.Second)
	cached, err := cache.Load(context.Background(), &instance.Config{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	refreshed, err := cache.Load(context.Background(), &instance.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if loads != 2 || first.Total != 1 || cached.Total != 1 || refreshed.Total != 2 {
		t.Fatalf("loads = %d, totals = %d/%d/%d, want two loads with one cached result",
			loads, first.Total, cached.Total, refreshed.Total)
	}
}

func TestStatusParkedBacklogRequiresARepository(t *testing.T) {
	if _, err := queryStatusParkedBacklog(context.Background(), &instance.Config{}); err == nil {
		t.Fatal("queryStatusParkedBacklog: want an error with no repository configured")
	}
}
