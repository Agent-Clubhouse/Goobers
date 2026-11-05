package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/prstatus"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

type baseVerdictFixture struct {
	remediationProvider
	mu                  sync.Mutex
	live                providers.PullRequestSummary
	status              *providers.CheckDetail
	files, moved        []providers.ChangedFile
	readErr, compareErr error
	writes              []providers.PullRequestStatusRequest
	reads               int
	onCompare           func()
	onWrite             func()
	afterFirstRead      func()
}

func (f *baseVerdictFixture) GetPullRequest(context.Context, providers.RepositoryRef, string) (providers.PullRequestSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.reads == 2 && f.afterFirstRead != nil {
		f.afterFirstRead()
	}
	return f.live, nil
}
func (f *baseVerdictFixture) PullRequestFiles(context.Context, providers.RepositoryRef, string) ([]providers.ChangedFile, error) {
	return f.files, nil
}
func (f *baseVerdictFixture) CompareCommits(context.Context, providers.RepositoryRef, string, string) (providers.CompareResult, error) {
	if f.onCompare != nil {
		f.onCompare()
	}
	return providers.CompareResult{Files: f.moved}, f.compareErr
}
func (f *baseVerdictFixture) MergeReviewStatus(context.Context, providers.RepositoryRef, string) (*providers.CheckDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == nil {
		return nil, f.readErr
	}
	copy := *f.status
	return &copy, f.readErr
}
func (f *baseVerdictFixture) PublishPullRequestStatus(_ context.Context, req providers.PullRequestStatusRequest) (providers.PullRequestStatusResult, error) {
	if f.onWrite != nil {
		f.onWrite()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, req)
	f.status = &providers.CheckDetail{State: req.State, Summary: req.Description}
	return providers.PullRequestStatusResult{}, nil
}
func newBaseVerdictFixture() *baseVerdictFixture {
	return &baseVerdictFixture{live: providers.PullRequestSummary{Number: 7, HeadSHA: "head", BaseSHA: "new", State: "open"}, status: &providers.CheckDetail{State: providers.CheckStatePassing, Summary: "pass for base old: reviewed"}, files: []providers.ChangedFile{{Path: "a.go"}}, moved: []providers.ChangedFile{{Path: "a.go"}}}
}
func baseReconciler(t *testing.T, f *baseVerdictFixture) *verdictBaseReconciler {
	t.Helper()
	return &verdictBaseReconciler{root: initDemo(t), repo: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "repo"}, reader: f, publisher: f}
}
func TestBaseVerdictWithdrawal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(*baseVerdictFixture)
		writes  int
		wantErr bool
	}{
		{name: "overlap", writes: 1},
		{name: "rename source", edit: func(f *baseVerdictFixture) {
			f.moved = []providers.ChangedFile{{Path: "renamed.go", PreviousPath: "a.go"}}
		}, writes: 1},
		{name: "disjoint", edit: func(f *baseVerdictFixture) { f.moved = []providers.ChangedFile{{Path: "b.go"}} }},
		{name: "fresh", edit: func(f *baseVerdictFixture) { f.status.Summary = "pass for base new: reviewed" }},
		{name: "no status", edit: func(f *baseVerdictFixture) { f.status = nil }},
		{name: "non pass", edit: func(f *baseVerdictFixture) { f.status.State = providers.CheckStateFailing }},
		{name: "malformed pass", edit: func(f *baseVerdictFixture) { f.status.Summary = "no pin" }, writes: 1},
		{name: "unreadable status", edit: func(f *baseVerdictFixture) { f.readErr = errors.New("unreadable") }, wantErr: true},
		{name: "unreadable delta", edit: func(f *baseVerdictFixture) { f.compareErr = errors.New("unreadable") }, writes: 1},
		{name: "head changed", edit: func(f *baseVerdictFixture) { f.afterFirstRead = func() { f.live.HeadSHA = "next" } }, wantErr: true},
		{name: "base changed", edit: func(f *baseVerdictFixture) { f.afterFirstRead = func() { f.live.BaseSHA = "next" } }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBaseVerdictFixture()
			if tc.edit != nil {
				tc.edit(f)
			}
			r := baseReconciler(t, f)
			err := r.reconcile(t.Context(), 7, io.Discard)
			if (err != nil) != tc.wantErr || len(f.writes) != tc.writes {
				t.Fatalf("writes=%v error=%v", f.writes, err)
			}
			if tc.writes > 0 {
				if f.writes[0].HeadSHA != "head" || f.writes[0].State != providers.CheckStatePending {
					t.Fatalf("wrong subject: %+v", f.writes[0])
				}
				if err := r.reconcile(t.Context(), 7, io.Discard); err != nil || len(f.writes) != 1 {
					t.Fatalf("repeat not idempotent: writes=%d err=%v", len(f.writes), err)
				}
			}
		})
	}
}
func TestBaseVerdictCustodyOrdersWithdrawalBeforeFreshPass(t *testing.T) {
	f := newBaseVerdictFixture()
	r := baseReconciler(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	f.onCompare = func() { close(entered); <-release }
	withdrawn := make(chan error, 1)
	go func() { withdrawn <- r.reconcile(t.Context(), 7, io.Discard) }()
	<-entered
	req, _ := prstatus.VerdictStatusRequest(r.repo, "7", "pass", "head", "new", "fresh", "")
	published := make(chan error, 1)
	go func() {
		published <- publishCurrentVerdictStatus(t.Context(), r.root, f, f, req, io.Discard, io.Discard)
	}()
	close(release)
	if err := <-withdrawn; err != nil {
		t.Fatal(err)
	}
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 2 || f.writes[0].State != providers.CheckStatePending || f.writes[1].State != providers.CheckStatePassing {
		t.Fatalf("wrong publication order: %+v", f.writes)
	}
	// Another delayed sweep sees the fresh pass and must preserve it.
	f.onCompare = nil
	if err := r.reconcile(t.Context(), 7, io.Discard); err != nil || len(f.writes) != 2 {
		t.Fatalf("fresh pass overwritten: %v %+v", err, f.writes)
	}
}
func TestBaseVerdictPublisherRechecksPinsUnderCustody(t *testing.T) {
	f := newBaseVerdictFixture()
	r := baseReconciler(t, f)
	req, _ := prstatus.VerdictStatusRequest(r.repo, "7", "pass", "head", "old", "stale", "")
	if err := publishCurrentVerdictStatus(t.Context(), r.root, f, f, req, io.Discard, io.Discard); err == nil || len(f.writes) != 0 {
		t.Fatalf("stale publication escaped: %v %+v", err, f.writes)
	}
}

type baseSweepFixture struct {
	remediationProvider
	ids     []int
	pages   []int
	listErr error
}

func (f *baseSweepFixture) ListPullRequests(_ context.Context, req providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error) {
	f.pages = append(f.pages, req.Page)
	if f.listErr != nil {
		return nil, f.listErr
	}
	start := min((req.Page-1)*req.Limit, len(f.ids))
	end := min(start+req.Limit, len(f.ids))
	var out []providers.PullRequestSummary
	for _, n := range f.ids[start:end] {
		out = append(out, providers.PullRequestSummary{Number: n})
	}
	return out, nil
}
func TestBaseVerdictSweepResumesAndDoesNotStarveBehindFailure(t *testing.T) {
	root := newVerdictTestRoot(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "repo"}
	f := &baseSweepFixture{ids: []int{1, 2, 3, 4}}
	var visited []int
	failures := 2
	action := func(n int) error {
		visited = append(visited, n)
		if n == 1 && failures > 0 {
			failures--
			return errors.New("retry")
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		_ = sweepNativeBaseVerdicts(t.Context(), root, repo, "main", 2, f, action)
	}
	if !reflect.DeepEqual(visited, []int{1, 2, 1, 3, 1, 4}) {
		t.Fatalf("lost/frozen sweep: %v", visited)
	}
	if !reflect.DeepEqual(f.pages, []int{1, 2}) {
		t.Fatalf("unexpected page reads: %v", f.pages)
	}
	// The persisted cursor wraps, and a new external base movement will be
	// noticed without any locally observed merge or queue-timeout record.
	if err := sweepNativeBaseVerdicts(t.Context(), root, repo, "main", 2, f, action); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.pages, []int{1, 2, 3, 1}) {
		t.Fatalf("did not wrap: %v", f.pages)
	}
}
func TestBaseVerdictSweepSingleItemBudgetIsFair(t *testing.T) {
	root := newVerdictTestRoot(t)
	repo := providers.RepositoryRef{Name: "repo"}
	f := &baseSweepFixture{ids: []int{1, 2, 3}}
	var visited []int
	action := func(n int) error {
		visited = append(visited, n)
		if n == 1 {
			return errors.New("retry")
		}
		return nil
	}
	for i := 0; i < 5; i++ {
		_ = sweepNativeBaseVerdicts(t.Context(), root, repo, "main", 1, f, action)
	}
	if !reflect.DeepEqual(visited, []int{1, 1, 2, 1, 3}) {
		t.Fatalf("single-item budget starved: %v", visited)
	}
}
func TestBaseVerdictSweepPersistsPendingAcrossBudgetChange(t *testing.T) {
	root := newVerdictTestRoot(t)
	repo := providers.RepositoryRef{Name: "repo"}
	f := &baseSweepFixture{ids: []int{1, 2, 3, 4, 5, 6}}
	var visited []int
	action := func(n int) error { visited = append(visited, n); return nil }
	for _, budget := range []int{2, 1, 1, 2} {
		if err := sweepNativeBaseVerdicts(t.Context(), root, repo, "main", budget, f, action); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(visited, []int{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("budget change skipped work: %v", visited)
	}
}

// Exercise both registered callers with real provider HTTP and the separately
// injected review credential, including a public reviewed PR outside the bot
// branch namespace. Label fan-out must remain restricted to managed branches.
func TestBaseVerdictProductionCallers(t *testing.T) {
	for _, trigger := range []string{"fanout", "sweep"} {
		t.Run(trigger, func(t *testing.T) {
			root, server := setupMergeReviewStatusRun(t, apiv1.Verdict{Decision: apiv1.VerdictPass}, "reviewed-head", "new")
			server.addIssue(11, "Public reviewed contribution")
			server.addOpenPR(11, "contributor/topic", "main", "sibling-head", "new", false, nil, []fakePRFile{{path: "widget.go", status: "modified", patch: "@@ -1 +1 @@\n-a\n+b"}})
			server.seedCommitStatus("sibling-head", fakeCommitStatus{context: providers.MergeReviewStatusContext, state: "success", description: "pass for base old: reviewed"})
			server.mu.Lock()
			server.compares["old...new"] = fakeCompare{mergeBaseSHA: "old", files: []fakePRFile{{path: "widget.go", status: "modified"}}}
			server.mu.Unlock()
			repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
			if trigger == "fanout" {
				_, _, errs := fanOutNeedsRemediation(t.Context(), server.newGitHubProvider("write-token"), repo, root, 10, "main", io.Discard)
				if len(errs) > 0 {
					t.Fatal(errs)
				}
			} else {
				t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "issues-token")
				t.Setenv("GOOBERS_CRED_GITHUB_BRANCH_DELETE", "delete-token")
				t.Setenv("GOOBERS_INPUT_BASE", "main")
				if code, stdout, stderr := runArgs(t, "reconcile-post-merge", root); code != 0 {
					t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
				}
			}
			statuses := server.postedStatuses()
			if len(statuses) != 2 || statuses[1].state != "pending" || statuses[1].token != "review-token" {
				t.Fatalf("caller did not withdraw with review identity: %+v", statuses)
			}
			if issueHasLabel(server, 11, needsRemediationLabel) {
				t.Fatal("native withdrawal widened managed label fan-out")
			}
		})
	}
}

type verdictSweepFailedCheckpoint struct {
	stateclient.Store
	writes int
}

func (s *verdictSweepFailedCheckpoint) Put(ctx context.Context, key string, data []byte, etag string) (stateclient.Value, error) {
	s.writes++
	if s.writes == 2 {
		return stateclient.Value{}, errors.New("checkpoint unavailable")
	}
	return s.Store.Put(ctx, key, data, etag)
}
func TestBaseVerdictSweepReplaysAfterFailedCheckpoint(t *testing.T) {
	root := newVerdictTestRoot(t)
	repo := providers.RepositoryRef{Name: "repo"}
	f := &baseSweepFixture{ids: []int{1, 2}}
	var visited []int
	open := openHeldStageStateStore
	openHeldStageStateStore = func(layout instance.Layout) (stateclient.Store, error) {
		store, err := open(layout)
		return &verdictSweepFailedCheckpoint{Store: store}, err
	}
	t.Cleanup(func() { openHeldStageStateStore = open })
	action := func(n int) error { visited = append(visited, n); return nil }
	if err := sweepNativeBaseVerdicts(t.Context(), root, repo, "main", 2, f, action); err == nil {
		t.Fatal("expected checkpoint failure")
	}
	openHeldStageStateStore = open
	if err := sweepNativeBaseVerdicts(t.Context(), root, repo, "main", 2, f, action); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(visited, []int{1, 1, 2}) {
		t.Fatalf("lost work after successful effect / failed checkpoint: %v", visited)
	}
}
