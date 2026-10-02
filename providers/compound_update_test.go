package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression coverage for #2657: a compound work-item update (fields, labels,
// close, comment) that fails part-way through must be safe to retry — the
// retry posts no second comment, replays no effect an earlier attempt
// finished, and the failure names what committed and what did not.

// faultInjector fails the (skip+1)th request matching method and path with a
// 502. With commit set the request reaches the backend first, modelling a
// forge that committed the write but whose response was lost. It also counts
// mutating requests and stamps each issue read with an updated_at that moves
// on every committed mutation, so revisions behave like the real forge's.
type faultInjector struct {
	mu        sync.Mutex
	next      http.Handler
	issuePath string
	method    string
	path      string
	skip      int
	commit    bool
	fired     bool
	mutations int
}

func (f *faultInjector) arm(method, path string, skip int, commit bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.method, f.path, f.skip, f.commit, f.fired = method, path, skip, commit, false
}

func (f *faultInjector) mutationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mutations
}

func (f *faultInjector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	inject := false
	if !f.fired && f.method == r.Method && f.path == r.URL.Path {
		if f.skip == 0 {
			inject, f.fired = true, true
		} else {
			f.skip--
		}
	}
	commit := f.commit
	if r.Method != http.MethodGet && (!inject || commit) {
		f.mutations++
	}
	revision := f.mutations
	f.mu.Unlock()

	if inject && !commit {
		http.Error(w, "injected failure", http.StatusBadGateway)
		return
	}
	rec := httptest.NewRecorder()
	f.next.ServeHTTP(rec, r)
	if inject {
		http.Error(w, "injected failure after commit", http.StatusBadGateway)
		return
	}
	body := rec.Body.Bytes()
	if r.Method == http.MethodGet && r.URL.Path == f.issuePath && rec.Code == http.StatusOK {
		var issue map[string]interface{}
		if err := json.Unmarshal(body, &issue); err == nil {
			issue["updated_at"] = time.Date(2026, 1, 1, 0, 0, revision, 0, time.UTC).Format(time.RFC3339)
			body, _ = json.Marshal(issue)
		}
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(body)
}

type compoundUpdater interface {
	GetWorkItem(context.Context, RepositoryRef, string) (WorkItem, error)
	UpdateWorkItem(context.Context, UpdateWorkItemRequest) (WorkItem, error)
	UpdateWorkItemStatus(context.Context, UpdateWorkItemStatusRequest) (WorkItem, error)
}

// compoundBackend is one forge under test: its provider, injector, the API
// path prefix, and a reader for the comment bodies it holds.
type compoundBackend struct {
	provider compoundUpdater
	injector *faultInjector
	prefix   string
	comments func() []string
}

func newCompoundBackends(t *testing.T, rec MutationRecorder) map[string]func() compoundBackend {
	return map[string]func() compoundBackend{
		"github": func() compoundBackend {
			m := newIssueMock()
			inj := &faultInjector{next: m.handler(t), issuePath: "/repos/acme/app/issues/7"}
			srv := httptest.NewServer(inj)
			t.Cleanup(srv.Close)
			p := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = srv.URL }, WithMaxTransientRetries(0), WithMutationRecorder(rec))
			return compoundBackend{provider: p, injector: inj, prefix: "/repos/acme/app/issues/7", comments: func() []string {
				m.mu.Lock()
				defer m.mu.Unlock()
				return commentBodies(m.comments)
			}}
		},
		"gitea": func() compoundBackend {
			m := newGiteaIssueMock()
			m.labelCatalog[2] = LabelNeedsHuman
			inj := &faultInjector{next: m.handler(t), issuePath: "/api/v1/repos/acme/app/issues/7"}
			srv := httptest.NewServer(inj)
			t.Cleanup(srv.Close)
			p := NewGiteaProvider(srv.URL, "token", WithGiteaMaxTransientRetries(0), WithGiteaMutationRecorder(rec))
			return compoundBackend{provider: p, injector: inj, prefix: "/api/v1/repos/acme/app/issues/7", comments: func() []string {
				m.mu.Lock()
				defer m.mu.Unlock()
				return commentBodies(m.comments)
			}}
		},
	}
}

func commentBodies(comments []map[string]interface{}) []string {
	out := make([]string, 0, len(comments))
	for _, c := range comments {
		out = append(out, fmt.Sprint(c["body"]))
	}
	return out
}

func (b compoundBackend) labelRemovePath() string {
	if strings.HasPrefix(b.prefix, "/api/v1") {
		return b.prefix + "/labels/1" // gitea addresses route/backend by id
	}
	return b.prefix + "/labels/route/backend"
}

// TestCompoundUpdateRetryIsIdempotent fails a park-shaped update (close, swap
// labels, comment) after each sub-operation, then retries it, on both REST
// forges: exactly one comment results, the final state is the requested one,
// and the first failure reports what committed.
func TestCompoundUpdateRetryIsIdempotent(t *testing.T) {
	type fault struct {
		name          string
		method, path  string // path suffix after the issue prefix; "-" = label remove
		skip          int
		commit        bool
		wantErr       bool
		wantCompleted []string
		wantPending   []string
		// noReplay: the comment committed on the first attempt, so the retry
		// must find its marker and send no mutation at all.
		noReplay bool
	}
	faults := []fault{
		// The first effect's write committed but its response was lost:
		// nothing is known-complete, yet the error is still typed partial.
		{name: "field patch lost after commit", method: http.MethodPatch, commit: true, wantErr: true,
			wantCompleted: []string{}, wantPending: []string{"fields", "labels", "comment"}},
		{name: "label add rejected", method: http.MethodPost, path: "/labels", wantErr: true,
			wantCompleted: []string{"fields"}, wantPending: []string{"labels", "comment"}},
		{name: "label remove rejected", method: http.MethodDelete, path: "-", wantErr: true,
			wantCompleted: []string{"fields"}, wantPending: []string{"labels", "comment"}},
		{name: "comment rejected", method: http.MethodPost, path: "/comments", wantErr: true,
			wantCompleted: []string{"fields", "labels"}, wantPending: []string{"comment"}},
		{name: "comment response lost after commit", method: http.MethodPost, path: "/comments", commit: true, noReplay: true},
		{name: "final read fails after comment", method: http.MethodGet, skip: 1, wantErr: true,
			wantCompleted: []string{"fields", "labels", "comment"}, wantPending: []string{}, noReplay: true},
	}
	for _, f := range faults {
		for backendName, newBackend := range newCompoundBackends(t, &recordingRecorder{}) {
			t.Run(backendName+"/"+f.name, func(t *testing.T) {
				b := newBackend()
				ctx := context.Background()
				repo := RepositoryRef{Owner: "acme", Name: "app"}
				before, err := b.provider.GetWorkItem(ctx, repo, "7")
				if err != nil {
					t.Fatal(err)
				}
				req := UpdateWorkItemRequest{
					Repository: repo, ID: "7", ExpectedRevision: before.Revision,
					State: "closed", AddLabels: []string{LabelNeedsHuman}, RemoveLabels: []string{"route/backend"},
					Comment: "parked: needs a decision", IdempotencyKey: "issue-close-out/run-1/7",
				}
				path := b.prefix + f.path
				if f.path == "-" {
					path = b.labelRemovePath()
				}
				b.injector.arm(f.method, path, f.skip, f.commit)

				_, err = b.provider.UpdateWorkItem(ctx, req)
				if (err != nil) != f.wantErr {
					t.Fatalf("first attempt err = %v, wantErr %v", err, f.wantErr)
				}
				if f.wantErr {
					var partial *PartialUpdateError
					if !errors.As(err, &partial) {
						t.Fatalf("first attempt err = %v, want *PartialUpdateError", err)
					}
					if !reflect.DeepEqual(partial.Completed, f.wantCompleted) || !reflect.DeepEqual(partial.Pending, f.wantPending) {
						t.Fatalf("partial = completed %v pending %v, want %v / %v", partial.Completed, partial.Pending, f.wantCompleted, f.wantPending)
					}
				}

				mutationsBeforeRetry := b.injector.mutationCount()
				// The retry carries the same request, including the revision
				// read before the first attempt — which that attempt moved.
				item, err := b.provider.UpdateWorkItem(ctx, req)
				if !f.noReplay {
					// The comment never committed, so the provider cannot tell
					// the first attempt's own writes from a foreign one and the
					// compare-and-set guard refuses. The caller re-reads and
					// retries under the same key, as decomposition does.
					var conflict *RevisionConflictError
					if !errors.As(err, &conflict) {
						t.Fatalf("stale-revision retry err = %v, want *RevisionConflictError", err)
					}
					fresh, rerr := b.provider.GetWorkItem(ctx, repo, "7")
					if rerr != nil {
						t.Fatal(rerr)
					}
					req.ExpectedRevision = fresh.Revision
					item, err = b.provider.UpdateWorkItem(ctx, req)
				}
				if err != nil {
					t.Fatalf("retry: %v", err)
				}
				if f.noReplay && b.injector.mutationCount() != mutationsBeforeRetry {
					t.Fatalf("retry sent %d mutation(s); an update whose comment committed must replay nothing", b.injector.mutationCount()-mutationsBeforeRetry)
				}
				comments := b.comments()
				if len(comments) != 1 {
					t.Fatalf("comments = %q, want exactly one", comments)
				}
				if !containsExactLine(comments[0], OperationCommentMarker(req.IdempotencyKey)) {
					t.Fatalf("comment %q lacks the operation marker", comments[0])
				}
				if item.State != "closed" || !item.HasLabel(LabelNeedsHuman) || item.HasLabel("route/backend") {
					t.Fatalf("final item = state %q labels %v, want closed with needs-human and without route/backend", item.State, item.Labels)
				}
			})
		}
	}
}

// TestCompoundUpdateRetryDoesNotUndoLaterEdits: once a keyed update has fully
// applied, re-running it (a stage resumed after a crash) must not re-apply a
// label a human has since removed.
func TestCompoundUpdateRetryDoesNotUndoLaterEdits(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	req := UpdateWorkItemRequest{
		Repository: repo, ID: "7", AddLabels: []string{LabelNeedsHuman},
		Comment: "parked", IdempotencyKey: "issue-close-out/run-2/7",
	}
	if _, err := p.UpdateWorkItem(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.labels = []string{"route/backend"} // a human un-parks the item
	m.mu.Unlock()
	item, err := p.UpdateWorkItem(context.Background(), req)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if item.HasLabel(LabelNeedsHuman) {
		t.Fatalf("re-run re-applied a label a human removed: %v", item.Labels)
	}
	if len(m.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(m.comments))
	}
}

// TestCompoundUpdateIgnoresOtherKeysAndUnkeyedComments: only this update's
// own marker suppresses its comment.
func TestCompoundUpdateIgnoresOtherKeysAndUnkeyedComments(t *testing.T) {
	m := newIssueMock()
	p, repo := newIssueProvider(t, m)
	for _, req := range []UpdateWorkItemRequest{
		{Repository: repo, ID: "7", Comment: "first", IdempotencyKey: "issue-close-out/run-a/7"},
		{Repository: repo, ID: "7", Comment: "second", IdempotencyKey: "issue-close-out/run-b/7"},
		{Repository: repo, ID: "7", Comment: "unkeyed"},
		{Repository: repo, ID: "7", Comment: "unkeyed"},
	} {
		if _, err := p.UpdateWorkItem(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if got := commentBodies(m.comments); len(got) != 4 {
		t.Fatalf("comments = %q, want 4 (distinct keys and unkeyed updates never dedupe)", got)
	}
}

// TestCompoundStatusUpdateRetryIsIdempotent covers UpdateWorkItemStatus, the
// close-out's done/in-review path, failing after each sub-operation: a retry
// leaves one comment and the item closed with its status label.
func TestCompoundStatusUpdateRetryIsIdempotent(t *testing.T) {
	faults := []struct {
		name          string
		method, path  string
		skip          int
		commit        bool
		wantErr       bool
		wantCompleted []string
	}{
		{name: "label swap rejected", method: http.MethodPost, path: "/labels", wantErr: true, wantCompleted: []string{}},
		{name: "close rejected", method: http.MethodPatch, wantErr: true, wantCompleted: []string{"labels"}},
		{name: "comment rejected", method: http.MethodPost, path: "/comments", wantErr: true, wantCompleted: []string{"labels", "state"}},
		{name: "comment response lost after commit", method: http.MethodPost, path: "/comments", commit: true},
		{name: "final read fails after comment", method: http.MethodGet, skip: 1, wantErr: true, wantCompleted: []string{"labels", "state", "comment"}},
	}
	for _, f := range faults {
		for backendName, newBackend := range newCompoundBackends(t, &recordingRecorder{}) {
			t.Run(backendName+"/"+f.name, func(t *testing.T) {
				b := newBackend()
				ctx := context.Background()
				req := UpdateWorkItemStatusRequest{
					Repository: RepositoryRef{Owner: "acme", Name: "app"}, ID: "7", Status: WorkItemStatusDone,
					Comment: "Implemented in https://example.test/pr/1.", IdempotencyKey: "issue-close-out/run-3/7/done",
				}
				b.injector.arm(f.method, b.prefix+f.path, f.skip, f.commit)
				_, err := b.provider.UpdateWorkItemStatus(ctx, req)
				if (err != nil) != f.wantErr {
					t.Fatalf("first attempt err = %v, wantErr %v", err, f.wantErr)
				}
				if f.wantErr {
					var partial *PartialUpdateError
					if !errors.As(err, &partial) {
						t.Fatalf("first attempt err = %v, want *PartialUpdateError", err)
					}
					if !reflect.DeepEqual(partial.Completed, f.wantCompleted) {
						t.Fatalf("completed = %v, want %v", partial.Completed, f.wantCompleted)
					}
				}
				item, err := b.provider.UpdateWorkItemStatus(ctx, req)
				if err != nil {
					t.Fatalf("retry: %v", err)
				}
				if got := b.comments(); len(got) != 1 {
					t.Fatalf("comments = %q, want exactly one", got)
				}
				if item.State != "closed" || !item.HasLabel(statusLabel(WorkItemStatusDone)) {
					t.Fatalf("final item = state %q labels %v", item.State, item.Labels)
				}
			})
		}
	}
}

// TestPartialUpdateRecordsCommittedEffects: the mutation journal hears about
// effects that committed before a failure, not only after a clean finish.
func TestPartialUpdateRecordsCommittedEffects(t *testing.T) {
	rec := &recordingRecorder{}
	b := newCompoundBackends(t, rec)["github"]()
	b.injector.arm(http.MethodPost, b.prefix+"/comments", 0, false)
	_, err := b.provider.UpdateWorkItem(context.Background(), UpdateWorkItemRequest{
		Repository: RepositoryRef{Owner: "acme", Name: "app"}, ID: "7", State: "closed",
		AddLabels: []string{LabelNeedsHuman}, Comment: "parked", IdempotencyKey: "k",
	})
	if err == nil {
		t.Fatal("want an error")
	}
	ref, ok := rec.last()
	if !ok {
		t.Fatal("no mutation recorded for the committed effects")
	}
	if _, ok := ref.Fields["state"]; !ok {
		t.Fatalf("recorded fields %v lack the committed state change", ref.Fields)
	}
	if _, ok := ref.Fields["labels"]; !ok {
		t.Fatalf("recorded fields %v lack the committed label change", ref.Fields)
	}
	if _, ok := ref.Fields["comment"]; ok {
		t.Fatalf("recorded fields %v claim the comment that failed", ref.Fields)
	}
}

// TestUnkeyedUpdateKeepsCommentBeforeLabels: an update without an
// IdempotencyKey cannot dedupe its comment, so it keeps the established
// comment-then-labels order — a failed comment leaves the labels unapplied
// rather than a label standing with no explanation.
func TestUnkeyedUpdateKeepsCommentBeforeLabels(t *testing.T) {
	for backendName, newBackend := range newCompoundBackends(t, &recordingRecorder{}) {
		t.Run(backendName, func(t *testing.T) {
			b := newBackend()
			repo := RepositoryRef{Owner: "acme", Name: "app"}
			b.injector.arm(http.MethodPost, b.prefix+"/comments", 0, false)
			_, err := b.provider.UpdateWorkItem(context.Background(), UpdateWorkItemRequest{
				Repository: repo, ID: "7", State: "closed", AddLabels: []string{LabelNeedsHuman}, Comment: "parked",
			})
			var partial *PartialUpdateError
			if !errors.As(err, &partial) {
				t.Fatalf("err = %v, want *PartialUpdateError", err)
			}
			if want := []string{"comment", "labels"}; !reflect.DeepEqual(partial.Pending, want) {
				t.Fatalf("pending = %v, want %v (comment before labels)", partial.Pending, want)
			}
			item, err := b.provider.GetWorkItem(context.Background(), repo, "7")
			if err != nil {
				t.Fatal(err)
			}
			if item.HasLabel(LabelNeedsHuman) {
				t.Fatalf("label applied although its comment failed: %v", item.Labels)
			}
		})
	}
}
