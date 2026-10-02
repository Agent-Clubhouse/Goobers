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
	if item.ExternalID != "99" || item.ActionCount != 2 || item.LastOperation != "merge" ||
		item.URL != "https://github.com/acme/app/pull/99" || item.Workflow != "merge-review" {
		t.Fatalf("work item = %#v", item)
	}

	actions, truncated, err := db.WorkItemActions(context.Background(), "github", "acme/app", "pr", "99")
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(actions) != 2 || actions[0].Operation != "merge" ||
		actions[1].Operation != "open" || actions[0].RunID != "run-2" {
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
