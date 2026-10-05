package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestReferencedFilePaths(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "full paths, bare names, and backticked",
			text: "Touches `internal/gate/gate.go` and daemon.go plus merge-review.yaml",
			want: []string{"internal/gate/gate.go", "daemon.go", "merge-review.yaml"},
		},
		{
			name: "strips leading ./ and dedupes",
			text: "see ./cmd/goobers/run.go and again cmd/goobers/run.go",
			want: []string{"cmd/goobers/run.go"},
		},
		{
			name: "ignores version strings and prose",
			text: "bump to v1.2.3, e.g. nothing here, schema v0.1",
			want: []string{},
		},
		{
			name: "recognizes yml/json/ts",
			text: "config.yml, api/schema.json, portal/app.tsx",
			want: []string{"config.yml", "api/schema.json", "portal/app.tsx"},
		},
		{
			name: "no false match on longer extension",
			text: "the word gopher and golang are not files",
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := referencedFilePaths(tc.text)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("referencedFilePaths(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestFileRefMatchesPath(t *testing.T) {
	cases := []struct {
		prPath string
		ref    string
		want   bool
	}{
		{"internal/runner/run.go", "internal/runner/run.go", true}, // exact
		{"internal/runner/run.go", "run.go", true},                 // bare basename suffix
		{"internal/runner/run.go", "runner/run.go", true},          // partial path suffix
		{"internal/runner/delegate.go", "gate.go", false},          // suffix must be /-aligned
		{"cmd/goobers/run.go", "internal/runner/run.go", false},    // different full path
		{"daemon.go", "daemon.go", true},                           // top-level exact
	}
	for _, tc := range cases {
		if got := fileRefMatchesPath(tc.prPath, tc.ref); got != tc.want {
			t.Errorf("fileRefMatchesPath(%q, %q) = %v, want %v", tc.prPath, tc.ref, got, tc.want)
		}
	}
}

func TestDistinctPRsTouchingRefs(t *testing.T) {
	touches := []openPRTouch{
		{number: 1, files: []string{"internal/gate/gate.go", "internal/gate/gate.go", "a.go"}},
		{number: 2, files: []string{"internal/gate/gate.go"}},
		{number: 3, files: []string{"cmd/goobers/backlogquery.go"}},
	}
	if got := distinctPRsTouchingRefs([]string{"gate.go"}, touches); got != 2 {
		t.Errorf("gate.go touched by = %d, want 2", got)
	}
	if got := distinctPRsTouchingRefs([]string{"backlogquery.go"}, touches); got != 1 {
		t.Errorf("backlogquery.go touched by = %d, want 1", got)
	}
	if got := distinctPRsTouchingRefs(nil, touches); got != 0 {
		t.Errorf("no refs touched by = %d, want 0", got)
	}
	if got := distinctPRsTouchingRefs([]string{"nowhere.go"}, touches); got != 0 {
		t.Errorf("nowhere.go touched by = %d, want 0", got)
	}
}

func TestDistinctPRsTouchingRefsEmptyTouches(t *testing.T) {
	if got := distinctPRsTouchingRefs([]string{"gate.go"}, nil); got != 0 {
		t.Fatalf("gate.go touched by empty PR set = %d, want 0", got)
	}
}

func item(id, title, body string) providers.WorkItem {
	return providers.WorkItem{ID: id, Title: title, Body: body}
}

func TestPartitionByContention(t *testing.T) {
	touches := []openPRTouch{
		{number: 10, files: []string{"internal/gate/gate.go"}},
		{number: 11, files: []string{"internal/gate/gate.go"}},
	}
	eligible := []providers.WorkItem{
		item("100", "clean older", "touches internal/telemetry/query.go"),
		item("101", "contested", "reworks internal/gate/gate.go"),
		item("102", "clean newer", "no file references at all"),
	}

	ordered, contested := partitionByContention(eligible, touches, 2)

	wantIDs := []string{"100", "102", "101"} // clean (FIFO) then contested
	var gotIDs []string
	for _, it := range ordered {
		gotIDs = append(gotIDs, it.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("order = %v, want %v", gotIDs, wantIDs)
	}
	if !reflect.DeepEqual(contested, []string{"101"}) {
		t.Fatalf("contested = %v, want [101]", contested)
	}
}

func TestPartitionByContentionAllContestedIsFIFOStable(t *testing.T) {
	// When every candidate is contested, order is unchanged (no starvation:
	// FIFO claiming still proceeds).
	touches := []openPRTouch{
		{number: 1, files: []string{"a.go"}},
		{number: 2, files: []string{"a.go"}},
	}
	eligible := []providers.WorkItem{
		item("1", "first", "a.go"),
		item("2", "second", "a.go"),
	}
	ordered, contested := partitionByContention(eligible, touches, 2)
	if ordered[0].ID != "1" || ordered[1].ID != "2" {
		t.Fatalf("order = %v, want stable [1 2]", []string{ordered[0].ID, ordered[1].ID})
	}
	if len(contested) != 2 {
		t.Fatalf("contested count = %d, want 2", len(contested))
	}
}

func TestPartitionByContentionBelowThresholdIsClean(t *testing.T) {
	// A file touched by only one PR is below the default 2-PR threshold, so
	// the candidate is not deprioritized.
	touches := []openPRTouch{{number: 1, files: []string{"a.go"}}}
	eligible := []providers.WorkItem{
		item("1", "one", "a.go"),
		item("2", "two", "b.go"),
	}
	ordered, contested := partitionByContention(eligible, touches, 2)
	if len(contested) != 0 {
		t.Fatalf("contested = %v, want none (below threshold)", contested)
	}
	if ordered[0].ID != "1" || ordered[1].ID != "2" {
		t.Fatalf("order = %v, want unchanged", []string{ordered[0].ID, ordered[1].ID})
	}
}

func TestPartitionByContentionThresholdBoundaryAndClamp(t *testing.T) {
	touches := []openPRTouch{{number: 1, files: []string{"a.go"}}}
	eligible := []providers.WorkItem{
		item("1", "contested first", "a.go"),
		item("2", "clean second", "b.go"),
	}

	for _, minPRs := range []int{1, 0, -2} {
		ordered, contested := partitionByContention(eligible, touches, minPRs)
		if got := []string{ordered[0].ID, ordered[1].ID}; !reflect.DeepEqual(got, []string{"2", "1"}) {
			t.Fatalf("minPRs %d order = %v, want [2 1]", minPRs, got)
		}
		if !reflect.DeepEqual(contested, []string{"1"}) {
			t.Fatalf("minPRs %d contested = %v, want [1]", minPRs, contested)
		}
	}
}

func TestPartitionByContentionDoesNotMutateOrAliasCallerSlice(t *testing.T) {
	eligible := []providers.WorkItem{
		item("1", "contested", "a.go"),
		item("2", "clean", "b.go"),
	}
	wantInput := append([]providers.WorkItem(nil), eligible...)

	ordered, _ := partitionByContention(eligible, []openPRTouch{{number: 1, files: []string{"a.go"}}}, 1)
	if !reflect.DeepEqual(eligible, wantInput) {
		t.Fatalf("eligible mutated = %+v, want %+v", eligible, wantInput)
	}
	ordered[0].ID = "changed"
	if eligible[1].ID != "2" {
		t.Fatalf("ordered aliases eligible: eligible[1].ID = %q, want 2", eligible[1].ID)
	}
}

func TestPartitionByContentionEmptyInput(t *testing.T) {
	ordered, contested := partitionByContention(nil, nil, 2)
	if len(ordered) != 0 || contested != nil {
		t.Fatalf("empty partition = (%v, %v), want empty order and nil contested IDs", ordered, contested)
	}
}

type recordingOpenPRTouchesProvider struct {
	pullRequests []providers.PullRequestSummary
	files        map[string][]providers.ChangedFile
	listErr      error
	fileErr      map[string]error
	calls        []string
	listRequests []providers.ListPullRequestsRequest
}

func (p *recordingOpenPRTouchesProvider) ListPullRequests(ctx context.Context, req providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error) {
	p.calls = append(p.calls, "list")
	p.listRequests = append(p.listRequests, req)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.pullRequests, p.listErr
}

func (p *recordingOpenPRTouchesProvider) PullRequestFiles(ctx context.Context, _ providers.RepositoryRef, pullID string) ([]providers.ChangedFile, error) {
	p.calls = append(p.calls, "files:"+pullID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.fileErr[pullID]; err != nil {
		return nil, err
	}
	return p.files[pullID], nil
}

func TestOpenPRTouchesQueryAndCallOrder(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}
	provider := &recordingOpenPRTouchesProvider{
		pullRequests: []providers.PullRequestSummary{{Number: 7}, {Number: 3}},
		files: map[string][]providers.ChangedFile{
			"7": {{Path: "a.go"}, {Path: "a.go"}},
			"3": {{Path: "b.go"}},
		},
	}
	t.Setenv("GOOBERS_BRANCH_NAMESPACE", "acme")

	got, err := openPRTouches(context.Background(), provider, repo, "release")
	if err != nil {
		t.Fatalf("openPRTouches: %v", err)
	}
	wantRequest := providers.ListPullRequestsRequest{
		Repository: repo, Base: "release", HeadPrefix: "acme/", SkipCheckState: true,
	}
	if !reflect.DeepEqual(provider.listRequests, []providers.ListPullRequestsRequest{wantRequest}) {
		t.Fatalf("list requests = %+v, want %+v", provider.listRequests, []providers.ListPullRequestsRequest{wantRequest})
	}
	if want := []string{"list", "files:7", "files:3"}; !reflect.DeepEqual(provider.calls, want) {
		t.Fatalf("calls = %v, want %v", provider.calls, want)
	}
	want := []openPRTouch{
		{number: 7, files: []string{"a.go", "a.go"}},
		{number: 3, files: []string{"b.go"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("touches = %+v, want %+v", got, want)
	}
	provider.files["7"][0].Path = "changed.go"
	if got[0].files[0] != "a.go" {
		t.Fatalf("touch paths alias provider response: got %q, want a.go", got[0].files[0])
	}
}

func TestOpenPRTouchesUsesDefaultBranchNamespace(t *testing.T) {
	provider := &recordingOpenPRTouchesProvider{}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}

	if _, err := openPRTouches(context.Background(), provider, repo, ""); err != nil {
		t.Fatalf("openPRTouches: %v", err)
	}
	if got := provider.listRequests[0].HeadPrefix; got != providers.DefaultBranchNamespace {
		t.Fatalf("head prefix = %q, want %q", got, providers.DefaultBranchNamespace)
	}
}

func TestOpenPRTouchesFailuresReturnNoPartialEvidence(t *testing.T) {
	listFailure := errors.New("list failed")
	fileFailure := errors.New("files failed")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}

	for _, tc := range []struct {
		name      string
		provider  *recordingOpenPRTouchesProvider
		wantErr   error
		wantCalls []string
	}{
		{
			name:      "list",
			provider:  &recordingOpenPRTouchesProvider{listErr: listFailure},
			wantErr:   listFailure,
			wantCalls: []string{"list"},
		},
		{
			name: "files after partial collection",
			provider: &recordingOpenPRTouchesProvider{
				pullRequests: []providers.PullRequestSummary{{Number: 1}, {Number: 2}},
				files:        map[string][]providers.ChangedFile{"1": {{Path: "first.go"}}},
				fileErr:      map[string]error{"2": fileFailure},
			},
			wantErr:   fileFailure,
			wantCalls: []string{"list", "files:1", "files:2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := openPRTouches(context.Background(), tc.provider, repo, "main")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want identity %v", err, tc.wantErr)
			}
			if got != nil {
				t.Fatalf("touches = %+v, want nil after failure", got)
			}
			if !reflect.DeepEqual(tc.provider.calls, tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", tc.provider.calls, tc.wantCalls)
			}
		})
	}
}

func TestOpenPRTouchesCancellationReturnsNoEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider := &recordingOpenPRTouchesProvider{
		pullRequests: []providers.PullRequestSummary{{Number: 1}},
	}

	got, err := openPRTouches(ctx, provider, providers.RepositoryRef{
		Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets",
	}, "main")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got != nil {
		t.Fatalf("touches = %+v, want nil after cancellation", got)
	}
	if !reflect.DeepEqual(provider.calls, []string{"list"}) {
		t.Fatalf("calls = %v, want [list]", provider.calls)
	}
}
