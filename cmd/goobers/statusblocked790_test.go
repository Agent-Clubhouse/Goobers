package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func TestStatusReportsIssuesParkedOnLearnedDependencies(t *testing.T) {
	tests := []struct {
		name    string
		records map[string]blockedRecord
		want    []string
	}{
		{
			name:    "zero",
			records: nil,
			want:    []string{"Issues parked on learned dependencies: 0"},
		},
		{
			name: "one",
			records: map[string]blockedRecord{
				"510": {Blockers: []string{"441"}},
			},
			want: []string{
				"Issues parked on learned dependencies: 1",
				"#510 blocked by #441",
			},
		},
		{
			name: "multiple",
			records: map[string]blockedRecord{
				"511": {Blockers: []string{"445"}},
				"510": {Blockers: []string{"442", "441"}},
			},
			want: []string{
				"Issues parked on learned dependencies: 2",
				"#510 blocked by #441, #442",
				"#511 blocked by #445",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := initDemo(t)
			if len(tt.records) > 0 {
				l := instance.NewLayout(root)
				if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := saveBlockedRecords(blockedRecordsPath(l), tt.records); err != nil {
					t.Fatal(err)
				}
			}

			code, stdout, stderr := runArgs(t, "status", root)
			if code != 0 {
				t.Fatalf("status: code = %d, stderr = %q", code, stderr)
			}
			previous := -1
			for _, want := range tt.want {
				at := strings.Index(stdout, want)
				if at == -1 {
					t.Fatalf("stdout = %q, want %q", stdout, want)
				}
				if at < previous {
					t.Fatalf("stdout = %q, want parked issues in stable order", stdout)
				}
				previous = at
			}
		})
	}
}

func TestStatusDropsResolvedRecordOnBacklogEligibilityRefresh(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if err := saveBlockedRecords(blockedRecordsPath(l), map[string]blockedRecord{
		blockedRecordKey(repo, "510"): {
			Repository: repo,
			ItemID:     "510",
			Blockers:   []string{"441"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(441, "resolved prerequisite")
	server.addIssue(510, "parked item")
	server.closeIssue(441)
	if _, err := refreshBlockedEligibility(context.Background(), l, provider, repo, nil); err != nil {
		t.Fatalf("refreshBlockedEligibility: %v", err)
	}

	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "Issues parked on learned dependencies: 0") {
		t.Fatalf("stdout = %q, want resolved record removed", stdout)
	}
	if strings.Contains(stdout, "#510") || strings.Contains(stdout, "#441") {
		t.Fatalf("stdout = %q, want no stale resolved record", stdout)
	}
}

func TestStatusPrunesResolvedBlockerOnBacklogEligibilityRefresh(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if err := saveBlockedRecords(blockedRecordsPath(l), map[string]blockedRecord{
		blockedRecordKey(repo, "510"): {
			Repository: repo,
			ItemID:     "510",
			Blockers:   []string{"441", "442"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	server, provider, repo := blockedFilterFixture(t)
	server.addIssue(441, "resolved prerequisite")
	server.addIssue(442, "open prerequisite")
	server.addIssue(510, "parked item")
	server.closeIssue(441)
	if _, err := refreshBlockedEligibility(context.Background(), l, provider, repo, nil); err != nil {
		t.Fatalf("refreshBlockedEligibility: %v", err)
	}

	code, stdout, stderr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("status: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "Issues parked on learned dependencies: 1") ||
		!strings.Contains(stdout, "#510 blocked by #442") {
		t.Fatalf("stdout = %q, want parked item with its still-open blocker", stdout)
	}
	if strings.Contains(stdout, "#441") {
		t.Fatalf("stdout = %q, want resolved blocker pruned", stdout)
	}
}
