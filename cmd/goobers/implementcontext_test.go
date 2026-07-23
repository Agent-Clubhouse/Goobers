package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestBuildImplementationHotFileMapIsBoundedAndDeterministic(t *testing.T) {
	openTouches := []openPRTouch{
		{number: 12, files: []string{"z.go", "shared.go", "shared.go"}},
		{number: 10, files: []string{"a.go", "shared.go"}},
	}
	conflicts := []implementationConflictTouch{
		{runID: "run-8", files: []string{"conflicted.go", "shared.go", "shared.go"}},
	}

	got := buildImplementationHotFileMap(openTouches, conflicts, 2)
	if got.OpenPullRequests != 2 || got.RecentConflictRuns != 1 ||
		got.ConflictLookbackDays != 30 || got.TotalFiles != 4 || !got.Truncated {
		t.Fatalf("hot-file map metadata = %+v, want 2 open PRs, 1 conflict run, 4 total files, truncated", got)
	}
	want := []implementationHotFile{
		{
			Path: "shared.go", PullRequestCount: 2, PullRequests: []int{10, 12},
			RecentConflictCount: 1, RecentConflictRuns: []string{"run-8"},
		},
		{
			Path: "a.go", PullRequestCount: 1, PullRequests: []int{10},
			RecentConflictRuns: []string{},
		},
	}
	if !reflect.DeepEqual(got.Files, want) {
		t.Fatalf("hot files = %+v, want %+v", got.Files, want)
	}
}

func TestBuildImplementationHotFileMapBoundsPullRequestsPerFile(t *testing.T) {
	touches := make([]openPRTouch, maxImplementationRefsPerHotFile+3)
	for i := range touches {
		touches[i] = openPRTouch{number: i + 1, files: []string{"shared.go"}}
	}
	conflicts := make([]implementationConflictTouch, maxImplementationRefsPerHotFile+3)
	for i := range conflicts {
		conflicts[i] = implementationConflictTouch{
			runID: "run-" + string(rune('a'+i)),
			files: []string{"shared.go"},
		}
	}

	got := buildImplementationHotFileMap(touches, conflicts, 1)
	if len(got.Files) != 1 {
		t.Fatalf("hot files = %+v, want one", got.Files)
	}
	file := got.Files[0]
	if file.PullRequestCount != len(touches) ||
		len(file.PullRequests) != maxImplementationRefsPerHotFile ||
		!file.PullRequestsTruncated ||
		file.RecentConflictCount != len(conflicts) ||
		len(file.RecentConflictRuns) != maxImplementationRefsPerHotFile ||
		!file.RecentConflictRunsTruncated {
		t.Fatalf("bounded hot file = %+v, want both counts %d with %d listed and truncation marked",
			file, len(touches), maxImplementationRefsPerHotFile)
	}
}

func TestGatherImplementContextWritesTaxonomyAndHotFileMap(t *testing.T) {
	root := initDemo(t)
	now := time.Now().UTC()
	seedImplementationConflict(t, root, "acme-web", "recent-conflict-run", now.Add(-24*time.Hour),
		"internal/runner/run.go", "internal/runner/history.go")
	seedImplementationConflict(t, root, "acme-web", "old-conflict-run", now.Add(-31*24*time.Hour),
		"internal/runner/old.go")
	seedImplementationConflict(t, root, "other-gaggle", "other-conflict-run", now.Add(-24*time.Hour),
		"internal/runner/other.go")
	if err := os.MkdirAll(
		filepath.Join(layoutFor(root).ForGaggle("acme-web").RunsDir(), "partial-run"),
		0o755,
	); err != nil {
		t.Fatalf("create partial run directory: %v", err)
	}

	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(10, "goobers/implementation/run-10", "main", "head10", "base",
		false, nil, []fakePRFile{{path: "internal/runner/run.go"}, {path: "cmd/goobers/main.go"}})
	server.addOpenPR(11, "goobers/implementation/run-11", "main", "head11", "base",
		false, nil, []fakePRFile{{path: "internal/runner/run.go"}, {path: "providers/github.go"}})
	server.addOpenPR(12, "human/change", "main", "head12", "base",
		false, nil, []fakePRFile{{path: "internal/runner/run.go"}})
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-implementation-context")
	t.Setenv("GOOBERS_GAGGLE", "acme-web")
	t.Setenv("GOOBERS_INPUT_MAXHOTFILES", "4")

	workDir := t.TempDir()
	t.Chdir(workDir)
	if code, stdout, stderr := runArgs(t, "gather-implement-context", root); code != 0 {
		t.Fatalf("gather-implement-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(workDir, implementationContextResultFile))
	if err != nil {
		t.Fatalf("read implementation context: %v", err)
	}
	var got implementationContext
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal implementation context: %v", err)
	}
	if got.SchemaVersion != "v1" || got.VerdictTaxonomy.ContractVersion != apiv1.StageContractVersion {
		t.Fatalf("context versions = (%q, %q), want (v1, %s)", got.SchemaVersion, got.VerdictTaxonomy.ContractVersion, apiv1.StageContractVersion)
	}
	wantClasses := []apiv1.FindingClass{
		apiv1.FindingRebaseNeeded,
		apiv1.FindingConflict,
		apiv1.FindingSubstantive,
		apiv1.FindingCrossPRBlocked,
	}
	var gotClasses []apiv1.FindingClass
	for _, findingClass := range got.VerdictTaxonomy.FindingClasses {
		gotClasses = append(gotClasses, findingClass.Class)
		if findingClass.Meaning == "" {
			t.Fatalf("finding class %q has no meaning", findingClass.Class)
		}
		if findingClass.Class == apiv1.FindingConflict &&
			findingClass.Meaning != "a rebase does not apply cleanly and requires conflict resolution" {
			t.Fatalf("conflict meaning = %q, want shipped rebase-conflict contract", findingClass.Meaning)
		}
	}
	if !reflect.DeepEqual(gotClasses, wantClasses) {
		t.Fatalf("finding classes = %v, want %v", gotClasses, wantClasses)
	}
	if got.HotFileMap.OpenPullRequests != 2 || got.HotFileMap.RecentConflictRuns != 1 ||
		got.HotFileMap.ConflictLookbackDays != 30 || got.HotFileMap.TotalFiles != 4 || got.HotFileMap.Truncated {
		t.Fatalf("hot-file map metadata = %+v, want 2 open PRs, 1 recent conflict run, 4 total files", got.HotFileMap)
	}
	wantHotFile := implementationHotFile{
		Path: "internal/runner/run.go", PullRequestCount: 2, PullRequests: []int{10, 11},
		RecentConflictCount: 1, RecentConflictRuns: []string{"recent-conflict-run"},
	}
	if len(got.HotFileMap.Files) != 4 || !reflect.DeepEqual(got.HotFileMap.Files[0], wantHotFile) {
		t.Fatalf("hot files = %+v, want first %+v among four", got.HotFileMap.Files, wantHotFile)
	}
	wantHistoryFile := implementationHotFile{
		Path: "internal/runner/history.go", PullRequests: []int{},
		RecentConflictCount: 1, RecentConflictRuns: []string{"recent-conflict-run"},
	}
	if !reflect.DeepEqual(got.HotFileMap.Files[3], wantHistoryFile) {
		t.Fatalf("historical conflict file = %+v, want %+v", got.HotFileMap.Files[3], wantHistoryFile)
	}
	filesRequests, checkStateRequests := server.requestCounts()
	if filesRequests != 2 || checkStateRequests != 0 {
		t.Fatalf("provider requests = files:%d checks:%d, want files:2 checks:0", filesRequests, checkStateRequests)
	}
}

func seedImplementationConflict(t *testing.T, root, gaggle, runID string, at time.Time, files ...string) {
	t.Helper()
	run, err := journal.Create(layoutFor(root).ForGaggle(gaggle).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", Gaggle: gaggle,
	}, nil, journal.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("create conflict run: %v", err)
	}
	data, err := json.Marshal(implementationConflictArtifact{
		Code:             "base_sync_conflict",
		ConflictingFiles: files,
	})
	if err != nil {
		t.Fatalf("marshal conflict artifact: %v", err)
	}
	if _, err := run.RecordStageArtifact(
		"local-ci",
		1,
		"",
		"local-ci/base-sync-conflict.json",
		data,
	); err != nil {
		t.Fatalf("record conflict artifact: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close conflict run: %v", err)
	}
}
