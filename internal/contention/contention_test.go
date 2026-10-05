package contention

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestReferencedFilePaths(t *testing.T) {
	tests := []struct {
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
			name: "strips leading ./ and deduplicates",
			text: "see ./cmd/goobers/run.go and again cmd/goobers/run.go",
			want: []string{"cmd/goobers/run.go"},
		},
		{
			name: "ignores version strings and prose",
			text: "bump to v1.2.3, e.g. nothing here, schema v0.1",
			want: []string{},
		},
		{
			name: "recognizes supported extensions",
			text: "config.yml, api/schema.json, portal/app.tsx",
			want: []string{"config.yml", "api/schema.json", "portal/app.tsx"},
		},
		{
			name: "does not match longer words",
			text: "the word gopher and golang are not files",
			want: []string{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := referencedFilePaths(test.text)
			if len(got) == 0 && len(test.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("referencedFilePaths(%q) = %v, want %v", test.text, got, test.want)
			}
		})
	}
}

func TestDistinctPullRequestsTouching(t *testing.T) {
	touches := []PullRequestTouch{
		{Number: 1, Files: []string{"internal/gate/gate.go", "internal/gate/gate.go", "a.go"}},
		{Number: 2, Files: []string{"internal/gate/gate.go"}},
		{Number: 3, Files: []string{"cmd/goobers/backlogquery.go"}},
	}
	tests := []struct {
		name string
		refs []string
		want int
	}{
		{name: "aligned basename suffix", refs: []string{"gate.go"}, want: 2},
		{name: "partial path suffix", refs: []string{"goobers/backlogquery.go"}, want: 1},
		{name: "not substring suffix", refs: []string{"ate.go"}, want: 0},
		{name: "empty refs", want: 0},
		{name: "missing ref", refs: []string{"nowhere.go"}, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := distinctPullRequestsTouching(test.refs, touches); got != test.want {
				t.Fatalf("distinctPullRequestsTouching(%v) = %d, want %d", test.refs, got, test.want)
			}
		})
	}
	if got := distinctPullRequestsTouching([]string{"gate.go"}, nil); got != 0 {
		t.Fatalf("empty touches = %d, want 0", got)
	}
}

func workItem(id, title, body string) providers.WorkItem {
	return providers.WorkItem{ID: id, Title: title, Body: body}
}

func TestStablePartition(t *testing.T) {
	touches := []PullRequestTouch{
		{Number: 10, Files: []string{"internal/gate/gate.go"}},
		{Number: 11, Files: []string{"internal/gate/gate.go"}},
	}
	items := []providers.WorkItem{
		workItem("100", "clean older", "touches internal/telemetry/query.go"),
		workItem("101", "contested", "reworks internal/gate/gate.go"),
		workItem("102", "clean newer", "no file references at all"),
		workItem("103", "contested tie", "also gate.go"),
	}

	ordered, contested := StablePartition(items, touches, 2)

	if got, want := itemIDs(ordered), []string{"100", "102", "101", "103"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if want := []string{"101", "103"}; !reflect.DeepEqual(contested, want) {
		t.Fatalf("contested = %v, want %v", contested, want)
	}
}

func TestStablePartitionThresholdsAndDuplicates(t *testing.T) {
	items := []providers.WorkItem{
		workItem("1", "contested first", "a.go"),
		workItem("2", "clean second", "b.go"),
	}
	touches := []PullRequestTouch{{Number: 1, Files: []string{"a.go", "a.go"}}}

	for _, minPullRequests := range []int{1, 0, -2} {
		ordered, contested := StablePartition(items, touches, minPullRequests)
		if got, want := itemIDs(ordered), []string{"2", "1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("minimum %d order = %v, want %v", minPullRequests, got, want)
		}
		if want := []string{"1"}; !reflect.DeepEqual(contested, want) {
			t.Fatalf("minimum %d contested = %v, want %v", minPullRequests, contested, want)
		}
	}

	ordered, contested := StablePartition(items, touches, 2)
	if got, want := itemIDs(ordered), []string{"1", "2"}; !reflect.DeepEqual(got, want) || len(contested) != 0 {
		t.Fatalf("below threshold = (%v, %v), want unchanged and uncontested", got, contested)
	}
}

func TestStablePartitionEmptyAndInputOwnership(t *testing.T) {
	ordered, contested := StablePartition(nil, nil, 2)
	if len(ordered) != 0 || contested != nil {
		t.Fatalf("empty partition = (%v, %v), want empty order and nil contested IDs", ordered, contested)
	}

	items := []providers.WorkItem{
		workItem("1", "contested", "a.go"),
		workItem("2", "clean", "b.go"),
	}
	wantInput := append([]providers.WorkItem(nil), items...)
	ordered, _ = StablePartition(items, []PullRequestTouch{{Number: 1, Files: []string{"a.go"}}}, 1)
	if !reflect.DeepEqual(items, wantInput) {
		t.Fatalf("items mutated = %+v, want %+v", items, wantInput)
	}
	ordered[0].ID = "changed"
	if items[1].ID != "2" {
		t.Fatalf("output aliases input: items[1].ID = %q, want 2", items[1].ID)
	}
}

func TestStablePartitionAllContestedIsStable(t *testing.T) {
	items := []providers.WorkItem{
		workItem("1", "first", "a.go"),
		workItem("2", "second", "a.go"),
	}
	ordered, contested := StablePartition(items, []PullRequestTouch{
		{Number: 1, Files: []string{"a.go"}},
		{Number: 2, Files: []string{"a.go"}},
	}, 2)
	if got, want := itemIDs(ordered), []string{"1", "2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if got, want := contested, []string{"1", "2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("contested = %v, want %v", got, want)
	}
}

func itemIDs(items []providers.WorkItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

type recordingProvider struct {
	pullRequests []providers.PullRequestSummary
	files        map[string][]providers.ChangedFile
	listErr      error
	fileErr      map[string]error
	calls        []string
	listRequests []providers.ListPullRequestsRequest
}

func (p *recordingProvider) ListPullRequests(ctx context.Context, req providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error) {
	p.calls = append(p.calls, "list")
	p.listRequests = append(p.listRequests, req)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.pullRequests, p.listErr
}

func (p *recordingProvider) PullRequestFiles(ctx context.Context, _ providers.RepositoryRef, pullID string) ([]providers.ChangedFile, error) {
	p.calls = append(p.calls, "files:"+pullID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.fileErr[pullID]; err != nil {
		return nil, err
	}
	return p.files[pullID], nil
}

func TestOpenPullRequestTouchesQueryNamespaceOrderAndOwnership(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}
	provider := &recordingProvider{
		pullRequests: []providers.PullRequestSummary{{Number: 7}, {Number: 3}},
		files: map[string][]providers.ChangedFile{
			"7": {{Path: "a.go"}, {Path: "a.go"}},
			"3": {{Path: "b.go"}},
		},
	}

	got, err := OpenPullRequestTouches(context.Background(), provider, repo, "release", "acme/")
	if err != nil {
		t.Fatalf("OpenPullRequestTouches: %v", err)
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
	want := []PullRequestTouch{
		{Number: 7, Files: []string{"a.go", "a.go"}},
		{Number: 3, Files: []string{"b.go"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("touches = %+v, want %+v", got, want)
	}
	provider.files["7"][0].Path = "changed.go"
	if got[0].Files[0] != "a.go" {
		t.Fatalf("touch paths alias provider response: got %q, want a.go", got[0].Files[0])
	}
}

func TestOpenPullRequestTouchesEmpty(t *testing.T) {
	provider := &recordingProvider{}
	got, err := OpenPullRequestTouches(context.Background(), provider, providers.RepositoryRef{}, "", "goobers/")
	if err != nil {
		t.Fatalf("OpenPullRequestTouches: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("touches = %+v, want empty", got)
	}
}

func TestOpenPullRequestTouchesFailuresReturnNoPartialEvidence(t *testing.T) {
	listFailure := errors.New("list failed")
	fileFailure := errors.New("files failed")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "widgets"}

	tests := []struct {
		name      string
		provider  *recordingProvider
		wantErr   error
		wantCalls []string
	}{
		{
			name:      "list",
			provider:  &recordingProvider{listErr: listFailure},
			wantErr:   listFailure,
			wantCalls: []string{"list"},
		},
		{
			name: "files after partial collection",
			provider: &recordingProvider{
				pullRequests: []providers.PullRequestSummary{{Number: 1}, {Number: 2}},
				files:        map[string][]providers.ChangedFile{"1": {{Path: "first.go"}}},
				fileErr:      map[string]error{"2": fileFailure},
			},
			wantErr:   fileFailure,
			wantCalls: []string{"list", "files:1", "files:2"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := OpenPullRequestTouches(context.Background(), test.provider, repo, "main", "goobers/")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want identity %v", err, test.wantErr)
			}
			if got != nil {
				t.Fatalf("touches = %+v, want nil after failure", got)
			}
			if !reflect.DeepEqual(test.provider.calls, test.wantCalls) {
				t.Fatalf("calls = %v, want %v", test.provider.calls, test.wantCalls)
			}
		})
	}
}

func TestOpenPullRequestTouchesCancellationReturnsNoEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider := &recordingProvider{pullRequests: []providers.PullRequestSummary{{Number: 1}}}

	got, err := OpenPullRequestTouches(ctx, provider, providers.RepositoryRef{}, "main", "goobers/")
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
