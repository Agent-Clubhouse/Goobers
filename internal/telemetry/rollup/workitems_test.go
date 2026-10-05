package rollup

import (
	"context"
	"testing"
	"time"
)

func TestWorkItemsGroupsMutationsAndReturnsActionTimeline(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		runID    string
		workflow string
		started  time.Time
	}{
		{"run-1", "implementation", start},
		{"run-2", "merge-review", start.Add(time.Hour)},
	} {
		if _, err := db.sql.Exec(`
			INSERT INTO runs (run_id, workflow, workflow_version, gaggle, status, started_at)
			VALUES (?, ?, 1, 'core', 'completed', ?)`,
			row.runID, row.workflow, formatTime(row.started)); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutation := range []struct {
		runID     string
		seq       int
		kind      string
		external  string
		url       string
		operation string
		at        time.Time
	}{
		{"run-1", 1, "issue", "42", "https://github.com/acme/app/issues/42", "claim", start.Add(time.Minute)},
		{"run-1", 2, "pr", "99", "https://github.com/acme/app/pull/99", "open", start.Add(2 * time.Minute)},
		{"run-2", 1, "pr", "99", "", "merge", start.Add(time.Hour + time.Minute)},
		{"run-2", 2, "pr", "99", "", "comment", start.Add(time.Hour + 2*time.Minute)},
	} {
		if _, err := db.sql.Exec(`
			INSERT INTO provider_mutations
				(run_id, seq, provider, kind, external_id, url, operation, occurred_at)
			VALUES (?, ?, 'github', ?, ?, NULLIF(?, ''), ?, ?)`,
			mutation.runID, mutation.seq, mutation.kind, mutation.external,
			mutation.url, mutation.operation, formatTime(mutation.at)); err != nil {
			t.Fatal(err)
		}
	}
	for _, attribution := range []struct {
		kind string
		id   string
	}{
		{"issue", "42"},
		{"pr", "99"},
	} {
		if _, err := db.sql.Exec(`
			INSERT INTO run_cost_attribution
				(run_id, provider, external_kind, external_id, relationship)
			VALUES ('run-1', 'github', ?, ?, 'touched')`,
			attribution.kind, attribution.id); err != nil {
			t.Fatal(err)
		}
	}

	items, hasMore, err := db.WorkItems(context.Background(), WorkItemQuery{Kind: "pr", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if hasMore || len(items) != 1 {
		t.Fatalf("items = %#v, hasMore = %v", items, hasMore)
	}
	item := items[0]
	if item.ExternalID != "99" || item.ActionCount != 3 || item.LastOperation != "comment" ||
		item.Outcome != "done" ||
		item.URL != "https://github.com/acme/app/pull/99" || item.Workflow != "merge-review" {
		t.Fatalf("work item = %#v", item)
	}

	actions, truncated, err := db.WorkItemActions(context.Background(), "github", "acme/app", "pr", "99")
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(actions) != 3 || actions[0].Operation != "comment" ||
		actions[0].Outcome != "done" || actions[1].Operation != "merge" ||
		actions[2].Operation != "open" || actions[0].RunID != "run-2" {
		t.Fatalf("actions = %#v, truncated = %v", actions, truncated)
	}
	related, err := db.RelatedPullRequests(context.Background(), "github", "acme/app", "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 1 || related[0].ExternalID != "99" ||
		related[0].Repository != "acme/app" {
		t.Fatalf("related pull requests = %#v", related)
	}
}

// TestRelatedPullRequestsADO guards #6797: an ADO work item is project-scoped
// (<org>/<project>) while its pull requests live in repository-scoped
// <org>/<project>/_git/<repo> URLs, so the PR prefix must be built from the
// project rather than from the issue's repository as if it were a PR's.
func TestRelatedPullRequestsADO(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if _, err := db.sql.Exec(`
		INSERT INTO runs (run_id, workflow, workflow_version, gaggle, status, started_at)
		VALUES ('run-1', 'implementation', 1, 'core', 'completed', ?)`,
		formatTime(start)); err != nil {
		t.Fatal(err)
	}
	const (
		issueURL     = "https://dev.azure.com/org/project/_workitems/edit/42"
		pullURL      = "https://dev.azure.com/org/project/_git/app/pullrequest/721"
		otherProject = "https://dev.azure.com/org/other/_git/app/pullrequest/722"
	)
	for _, mutation := range []struct {
		seq       int
		kind      string
		external  string
		url       string
		operation string
	}{
		{1, "issue", "42", issueURL, "claim"},
		{2, "pr", "721", pullURL, "open"},
		{3, "issue", "42", issueURL, "link-pr"},
		{4, "pr", "722", otherProject, "open"},
	} {
		if _, err := db.sql.Exec(`
			INSERT INTO provider_mutations
				(run_id, seq, provider, kind, external_id, url, operation, occurred_at)
			VALUES ('run-1', ?, 'ado', ?, ?, ?, ?, ?)`,
			mutation.seq, mutation.kind, mutation.external, mutation.url,
			mutation.operation, formatTime(start.Add(time.Duration(mutation.seq)*time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	for _, attribution := range []struct {
		kind, id, repository, url string
	}{
		{"issue", "42", "org/project", issueURL},
		{"pr", "721", "org/project/app", pullURL},
		{"pr", "722", "org/other/app", otherProject},
	} {
		if _, err := db.sql.Exec(`
			INSERT INTO run_cost_attribution
				(run_id, provider, repository, external_kind, external_id, url, relationship)
			VALUES ('run-1', 'ado', ?, ?, ?, ?, 'touched')`,
			attribution.repository, attribution.kind, attribution.id, attribution.url); err != nil {
			t.Fatal(err)
		}
	}

	related, err := db.RelatedPullRequests(context.Background(), "ado", "org/project", "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 1 || related[0].ExternalID != "721" ||
		related[0].Repository != "org/project/app" || related[0].URL != pullURL {
		t.Fatalf("related pull requests = %#v", related)
	}
}

func TestWorkItemsClassifiesIssueOutcomes(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, run := range []struct {
		id     string
		status string
		at     time.Time
	}{
		{"run-done", "completed", start},
		{"run-failed", "failed", start.Add(time.Hour)},
	} {
		if _, err := db.sql.Exec(`
			INSERT INTO runs (run_id, workflow, workflow_version, gaggle, status, started_at)
			VALUES (?, 'implementation', 1, 'core', ?, ?)`,
			run.id, run.status, formatTime(run.at)); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutation := range []struct {
		runID     string
		seq       int
		external  string
		operation string
		at        time.Time
	}{
		{"run-done", 1, "42", "close", start.Add(time.Minute)},
		{"run-done", 2, "42", "comment", start.Add(2 * time.Minute)},
		{"run-failed", 1, "77", "comment", start.Add(time.Hour + time.Minute)},
	} {
		if _, err := db.sql.Exec(`
			INSERT INTO provider_mutations
				(run_id, seq, provider, kind, external_id, url, operation, occurred_at)
			VALUES (?, ?, 'github', 'issue', ?, ?, ?, ?)`,
			mutation.runID, mutation.seq, mutation.external,
			"https://github.com/acme/app/issues/"+mutation.external,
			mutation.operation, formatTime(mutation.at)); err != nil {
			t.Fatal(err)
		}
	}

	items, _, err := db.WorkItems(context.Background(), WorkItemQuery{Kind: "issue", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %#v", items)
	}
	if items[0].ExternalID != "77" || items[0].Outcome != "bad-terminal" {
		t.Fatalf("failed item = %#v", items[0])
	}
	if items[1].ExternalID != "42" || items[1].LastOperation != "comment" ||
		items[1].Outcome != "done" {
		t.Fatalf("closed item = %#v", items[1])
	}
}

func TestWorkItemsKeepSameNumberedRepositoriesSeparate(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for index, repository := range []string{"acme/app", "acme/service"} {
		runID := "run-" + repository
		if _, err := db.sql.Exec(`
			INSERT INTO runs (run_id, workflow, workflow_version, gaggle, status, started_at)
			VALUES (?, 'implementation', 1, ?, 'completed', ?)`,
			runID, repository, formatTime(at.Add(time.Duration(index)*time.Minute))); err != nil {
			t.Fatal(err)
		}
		if _, err := db.sql.Exec(`
			INSERT INTO provider_mutations
				(run_id, seq, provider, kind, external_id, url, operation, occurred_at)
			VALUES (?, 1, 'github', 'issue', '42', ?, 'comment', ?)`,
			runID, "https://github.com/"+repository+"/issues/42#issuecomment-1",
			formatTime(at.Add(time.Duration(index)*time.Minute))); err != nil {
			t.Fatal(err)
		}
		if _, err := db.sql.Exec(`
			INSERT INTO provider_mutations
				(run_id, seq, provider, kind, external_id, url, operation, occurred_at)
			VALUES (?, 2, 'github', 'issue', '42', NULL, 'label', ?)`,
			runID, formatTime(at.Add(time.Duration(index)*time.Minute+time.Second))); err != nil {
			t.Fatal(err)
		}
	}

	items, _, err := db.WorkItems(context.Background(), WorkItemQuery{Kind: "issue", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Repository == items[1].Repository ||
		items[0].ActionCount != 2 || items[1].ActionCount != 2 {
		t.Fatalf("items = %#v", items)
	}
	actions, _, err := db.WorkItemActions(context.Background(), "github", "acme/app", "issue", "42")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0].RunID != "run-acme/app" ||
		actions[0].Operation != "label" {
		t.Fatalf("actions = %#v", actions)
	}
}

func TestWorkItemsPreserveSelfHostedADOAuthority(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if _, err := db.sql.Exec(`
		INSERT INTO runs (run_id, workflow, workflow_version, gaggle, status, started_at)
		VALUES ('run-ado', 'implementation', 1, 'ado-core', 'completed', ?)`,
		formatTime(at)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`
		INSERT INTO provider_mutations
			(run_id, seq, provider, kind, external_id, url, operation, occurred_at)
		VALUES ('run-ado', 1, 'ado', 'issue', '7',
			'https://tfs.corp.example:8080/org/proj/_workitems/edit/7#discussion',
			'update', ?)`, formatTime(at)); err != nil {
		t.Fatal(err)
	}

	items, _, err := db.WorkItems(context.Background(), WorkItemQuery{Provider: "ado", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 ||
		items[0].URL != "https://tfs.corp.example:8080/org/proj/_workitems/edit/7" ||
		items[0].Repository != "org/proj" {
		t.Fatalf("items = %#v", items)
	}

	actions, truncated, err := db.WorkItemActions(context.Background(), "ado", "org/proj", "issue", "7")
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(actions) != 1 || actions[0].URL != items[0].URL {
		t.Fatalf("actions = %#v, truncated = %v", actions, truncated)
	}
}

func TestWorkItemsGroupEquivalentADOAuthorities(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for index, mutation := range []struct {
		gaggle string
		url    string
	}{
		{"alpha", "https://dev.azure.com/org/proj/_workitems/edit/7"},
		{"beta", "https://org.visualstudio.com/proj/_workitems/edit/7"},
		{"gamma", ""},
	} {
		runID := "run-" + mutation.gaggle
		at := start.Add(time.Duration(index) * time.Minute)
		if _, err := db.sql.Exec(`
			INSERT INTO runs (run_id, workflow, workflow_version, gaggle, status, started_at)
			VALUES (?, 'implementation', 1, ?, 'completed', ?)`,
			runID, mutation.gaggle, formatTime(at)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.sql.Exec(`
			INSERT INTO provider_mutations
				(run_id, seq, provider, kind, external_id, url, operation, occurred_at)
			VALUES (?, 1, 'ado', 'issue', '7', NULLIF(?, ''), 'update', ?)`,
			runID, mutation.url, formatTime(at)); err != nil {
			t.Fatal(err)
		}
	}

	items, hasMore, err := db.WorkItems(context.Background(), WorkItemQuery{Provider: "ado", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if hasMore || len(items) != 1 {
		t.Fatalf("items = %#v, hasMore = %v", items, hasMore)
	}
	if items[0].Repository != "org/proj" ||
		items[0].URL != "https://org.visualstudio.com/proj/_workitems/edit/7" ||
		items[0].ActionCount != 3 || items[0].LastRunID != "run-gamma" {
		t.Fatalf("work item = %#v", items[0])
	}

	actions, truncated, err := db.WorkItemActions(context.Background(), "ado", "org/proj", "issue", "7")
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(actions) != 3 || actions[0].RunID != "run-gamma" {
		t.Fatalf("actions = %#v, truncated = %v", actions, truncated)
	}
}

func TestWorkItemsCanonicalizeURLVariantsAcrossGaggles(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for index, mutation := range []struct {
		gaggle string
		url    string
	}{
		{"alpha", "https://github.com/acme/app/issues/99#issuecomment-1"},
		{"beta", "https://api.github.com/repos/acme/app/issues/99"},
		{"gamma", ""},
	} {
		runID := "run-" + mutation.gaggle
		at := start.Add(time.Duration(index) * time.Minute)
		if _, err := db.sql.Exec(`
			INSERT INTO runs (run_id, workflow, workflow_version, gaggle, status, started_at)
			VALUES (?, 'implementation', 1, ?, 'completed', ?)`,
			runID, mutation.gaggle, formatTime(at)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.sql.Exec(`
			INSERT INTO provider_mutations
				(run_id, seq, provider, kind, external_id, url, operation, occurred_at)
			VALUES (?, 1, 'github', 'pr', '99', NULLIF(?, ''), 'update', ?)`,
			runID, mutation.url, formatTime(at)); err != nil {
			t.Fatal(err)
		}
	}

	items, hasMore, err := db.WorkItems(context.Background(), WorkItemQuery{Kind: "pr", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if hasMore || len(items) != 1 {
		t.Fatalf("items = %#v, hasMore = %v", items, hasMore)
	}
	item := items[0]
	if item.Repository != "acme/app" || item.URL != "https://github.com/acme/app/pull/99" ||
		item.ActionCount != 3 || item.LastRunID != "run-gamma" {
		t.Fatalf("work item = %#v", item)
	}

	actions, truncated, err := db.WorkItemActions(context.Background(), "github", "acme/app", "pr", "99")
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(actions) != 3 || actions[0].RunID != "run-gamma" {
		t.Fatalf("actions = %#v, truncated = %v", actions, truncated)
	}
}
