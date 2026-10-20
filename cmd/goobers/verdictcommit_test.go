package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/findingledger"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

var verdictCommitRepo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "repo"}

func TestVerdictReadUsesCommittedSnapshotAfterProjectionFailure(t *testing.T) {
	failure := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, HeadSHA: "head", Findings: []apiv1.Finding{unboundedLineageFinding()}}
	first, _ := findingledger.Commit(nil, failure, 1, prFindingPolicy(55))
	answer := apiv1.Verdict{Decision: apiv1.VerdictPass, HeadSHA: "head", Dispositions: []apiv1.FindingDisposition{{
		FindingID: first.Findings[0].ID, FindingGeneration: 1, Outcome: apiv1.FindingRejected, Reason: "Caller already bounds it.",
	}}}
	resolved, _ := findingledger.Commit([]apiv1.Verdict{first}, answer, 2, prFindingPolicy(55))
	// The first read raced the resolution, or recovered an old provider
	// projection. The records read now includes the authoritative snapshot.
	got, standing := carryOutstandingFindings([]apiv1.Verdict{first, resolved}, first, 55, true)
	if got.Decision != apiv1.VerdictPass || got.LedgerGeneration != 2 || len(got.Findings) != 0 || len(standing) != 0 {
		t.Fatalf("read resurrected resolved findings: %+v standing=%v", got, standing)
	}
}

func TestVerdictReadMigratesCompleteLegacyLedger(t *testing.T) {
	root := newVerdictTestRoot(t)
	comments := []providers.Comment{
		statusComment("reviewer", apiv1.Verdict{Decision: apiv1.VerdictPass, HeadSHA: "head"}),
		statusComment("reviewer", apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, HeadSHA: "head", Findings: []apiv1.Finding{unboundedLineageFinding()}}),
	}
	if _, _, err := gatherPRVerdictChecked(root, verdictCommitRepo, 55, comments, "reviewer"); err != nil {
		t.Fatal(err)
	}
	// Losing the provider projection after migration must not lose a finding
	// from a non-canonical legacy comment.
	got, standing, err := gatherPRVerdictChecked(root, verdictCommitRepo, 55, nil, "reviewer")
	if err != nil || got == nil || got.Decision != apiv1.VerdictNeedsChanges || len(standing) != 1 || standing[0] != unboundedLineageID {
		t.Fatalf("migration lost the legacy ledger: verdict=%+v standing=%v err=%v", got, standing, err)
	}
}

// verdictCommitPlane uses the real file store behind the HTTP CAS contract.
// Each caller gets an independent HTTP client, as separate stage pods do.
// beforePut lets a test force the exact stale-read/write interleaving.
func verdictCommitPlane(t *testing.T, beforePut func([]byte)) func() stateclient.Store {
	t.Helper()
	backend, err := openStageStateStore(instance.NewLayout(newVerdictTestRoot(t)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/api/v1/gaggles/test/state/")
		var value stateclient.Value
		var err error
		switch r.Method {
		case http.MethodGet:
			value, err = backend.Get(r.Context(), key)
			if err == nil && !value.Exists() {
				http.Error(w, `{"error":{"code":"not_found"}}`, http.StatusNotFound)
				return
			}
		case http.MethodPut:
			data, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Error(readErr)
			}
			if beforePut != nil {
				beforePut(data)
			}
			value, err = backend.Put(r.Context(), key, data, strings.Trim(r.Header.Get("If-Match"), `"`))
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
		if errors.Is(err, stateclient.ErrPreconditionFailed) {
			http.Error(w, `{"error":{"code":"precondition_failed"}}`, http.StatusPreconditionFailed)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", `"`+value.ETag+`"`)
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write(value.Data)
	}))
	t.Cleanup(server.Close)
	return func() stateclient.Store {
		client, err := stateclient.NewHTTP(stateclient.HTTPConfig{BaseURL: server.URL, Gaggle: "test", Token: "test-token"})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
}

func capturedVerdictPublication(t *testing.T, store stateclient.Store) verdictPublication {
	t.Helper()
	key := verdictRecordKey(verdictCommitRepo, 55)
	observed, err := store.Get(t.Context(), verdictStateKey(key))
	if err != nil {
		t.Fatal(err)
	}
	return verdictPublication{store: store, key: key, observed: observed, root: newVerdictTestRoot(t), number: 55}
}

func TestVerdictCommitRetriesAgainstConcurrentFinding(t *testing.T) {
	for _, secondFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale pass", true: "two distinct failures"}[secondFailure], func(t *testing.T) {
			blocked, release := make(chan struct{}), make(chan struct{})
			var held atomic.Bool
			clients := verdictCommitPlane(t, func(data []byte) {
				if bytes.Contains(data, []byte(`"summary":"delayed"`)) && held.CompareAndSwap(false, true) {
					close(blocked)
					<-release
				}
			})
			first, second := capturedVerdictPublication(t, clients()), capturedVerdictPublication(t, clients())
			delayed := apiv1.Verdict{Decision: apiv1.VerdictPass, HeadSHA: "head", BaseSHA: "base", Summary: "delayed"}
			if secondFailure {
				delayed.Decision = apiv1.VerdictNeedsChanges
				delayed.Findings = []apiv1.Finding{{ID: "second", Class: apiv1.FindingSubstantive, Severity: apiv1.SeverityError, Message: "Second defect."}}
			}
			type outcome struct {
				verdict apiv1.Verdict
				err     error
			}
			done := make(chan outcome, 1)
			go func() { v, err := first.commit(delayed, nil, nil, io.Discard); done <- outcome{v, err} }()
			<-blocked
			failure := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, HeadSHA: "head", BaseSHA: "base", Findings: []apiv1.Finding{unboundedLineageFinding()}}
			committedFailure, err := second.commit(failure, nil, nil, io.Discard)
			close(release)
			if err != nil {
				t.Fatal(err)
			}
			got := <-done
			if got.err != nil || got.verdict.Decision != apiv1.VerdictNeedsChanges || got.verdict.LedgerGeneration != committedFailure.LedgerGeneration+1 {
				t.Fatalf("stale writer did not receive its committed fold: %+v", got)
			}
			want := 1
			if secondFailure {
				want = 2
			}
			if open := findingledger.Outstanding([]apiv1.Verdict{got.verdict}, "head", prFindingPolicy(55)); len(open) != want {
				t.Fatalf("lost concurrent findings: %+v", got.verdict)
			}
			fresh := capturedVerdictPublication(t, clients())
			stored, exists, err := decodeVerdictRecord(fresh.observed, fresh.key)
			if err != nil || !exists || stored.LedgerGeneration != got.verdict.LedgerGeneration || stored.Decision != got.verdict.Decision {
				t.Fatalf("fresh client disagrees with committed return: %+v %v", stored, err)
			}
			if delayed.LedgerGeneration != 0 || len(delayed.StandingFindings) != 0 {
				t.Fatalf("retry mutated input: %+v", delayed)
			}
		})
	}
}

func TestVerdictCommitRejectsDelayedPreviousHead(t *testing.T) {
	blocked, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	clients := verdictCommitPlane(t, func(data []byte) {
		if bytes.Contains(data, []byte(`"summary":"delayed"`)) && held.CompareAndSwap(false, true) {
			close(blocked)
			<-release
		}
	})
	seed := apiv1.Verdict{Decision: apiv1.VerdictPass, HeadSHA: "old", BaseSHA: "base"}
	if _, err := capturedVerdictPublication(t, clients()).commit(seed, nil, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	old, replacement := capturedVerdictPublication(t, clients()), capturedVerdictPublication(t, clients())
	seed.Summary = "delayed"
	done := make(chan error, 1)
	go func() { _, err := old.commit(seed, nil, nil, io.Discard); done <- err }()
	<-blocked
	newHead := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, HeadSHA: "new", BaseSHA: "base", Findings: []apiv1.Finding{unboundedLineageFinding()}}
	_, err := replacement.commit(newHead, nil, nil, io.Discard)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "predecessor changed") {
		t.Fatalf("old head was allowed to replace the new head: %v", err)
	}
	fresh := capturedVerdictPublication(t, clients())
	stored, _, err := decodeVerdictRecord(fresh.observed, fresh.key)
	if err != nil || stored.HeadSHA != "new" || len(stored.Findings) != 1 {
		t.Fatalf("new-head finding lost: %+v %v", stored, err)
	}
}

type rejectedVerdictStore struct{ stateclient.Store }

func (rejectedVerdictStore) Update(context.Context, string, string, func(stateclient.Value) ([]byte, bool, error)) error {
	return errors.New("state write refused")
}

func TestVerdictCommitReturnsNoPublishableValueOnFailure(t *testing.T) {
	publication := capturedVerdictPublication(t, verdictCommitPlane(t, nil)())
	publication.store = rejectedVerdictStore{publication.store}
	got, err := publication.commit(apiv1.Verdict{Decision: apiv1.VerdictPass, HeadSHA: "head", BaseSHA: "base"}, nil, nil, io.Discard)
	if err == nil || got.Decision != "" || got.LedgerGeneration != 0 {
		t.Fatalf("failed commit returned publishable value: %+v %v", got, err)
	}
}

func TestADOReviewedVerdictCommitsBeforeStatusAndRoutesCommittedDecision(t *testing.T) {
	for _, rejectWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "concurrent finding", true: "state failure"}[rejectWrite], func(t *testing.T) {
			root := newVerdictTestRoot(t)
			repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}
			publication, err := beginVerdictPublication(root, repo, 55)
			if err != nil {
				t.Fatal(err)
			}
			other, err := beginVerdictPublication(root, repo, 55)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := other.commit(apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, HeadSHA: "head", BaseSHA: "base", Findings: []apiv1.Finding{unboundedLineageFinding()}}, nil, nil, io.Discard); err != nil {
				t.Fatal(err)
			}
			if rejectWrite {
				publication.store = rejectedVerdictStore{publication.store}
			}
			var statuses []string
			var thread apiv1.Verdict
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/connectionData"):
					_, _ = io.WriteString(w, `{"authenticatedUser":{"id":"reviewer","providerDisplayName":"reviewer"}}`)
				case strings.HasSuffix(r.URL.Path, "/iterations"):
					_, _ = io.WriteString(w, `{"value":[{"id":1,"sourceRefCommit":{"commitId":"head"}}]}`)
				case strings.HasSuffix(r.URL.Path, "/statuses"):
					stored, ok, err := loadVerdictState(root, repo, 55)
					if err != nil || !ok || stored.LedgerGeneration != 2 || stored.Decision != apiv1.VerdictNeedsChanges {
						t.Errorf("status reached provider before committed downgrade: %+v %v", stored, err)
					}
					var body struct{ State string }
					_ = json.NewDecoder(r.Body).Decode(&body)
					statuses = append(statuses, body.State)
					_, _ = io.WriteString(w, `{"id":7}`)
				case strings.HasSuffix(r.URL.Path, "/threads") && r.Method == http.MethodPost:
					var body struct{ Comments []struct{ Content string } }
					_ = json.NewDecoder(r.Body).Decode(&body)
					if len(body.Comments) > 0 {
						thread, _ = parseVerdictComment(body.Comments[0].Content)
					}
					_, _ = io.WriteString(w, `{"id":11,"comments":[{"id":1,"content":"posted"}]}`)
				case r.Method == http.MethodGet:
					_, _ = io.WriteString(w, `{"value":[]}`)
				case strings.HasSuffix(r.URL.Path, "/labels"):
					_, _ = io.WriteString(w, `{"id":"label"}`)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", http.StatusBadRequest)
				}
			}))
			defer server.Close()
			provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL })
			var stdout, stderr bytes.Buffer
			result := filepath.Join(root, "result.json")
			code := publishADOReviewedVerdict(t.Context(), root, provider, repo, 55, providers.PullRequestSummary{HeadSHA: "head", BaseSHA: "base"}, apiv1.Verdict{Decision: apiv1.VerdictPass}, publication, result, &stdout, &stderr)
			if rejectWrite {
				if code == 0 || len(statuses) != 0 || thread.Decision != "" {
					t.Fatalf("failed write published: code=%d statuses=%v thread=%+v stderr=%s", code, statuses, thread, stderr.String())
				}
				return
			}
			if code != 0 || len(statuses) != 1 || statuses[0] != "failed" || thread.Decision != apiv1.VerdictNeedsChanges || thread.LedgerGeneration != 2 {
				t.Fatalf("pre-commit pass escaped: code=%d statuses=%v thread=%+v stderr=%s", code, statuses, thread, stderr.String())
			}
			if got := readVerdictResult(t, result); got["decision"] != "needs-changes" {
				t.Fatalf("stage result lost committed decision: %v", got)
			}
		})
	}
}

type interposedVerdictStore struct {
	stateclient.Store
	before func()
}

func (s interposedVerdictStore) Update(ctx context.Context, key, operation string, fn func(stateclient.Value) ([]byte, bool, error)) error {
	if operation == verdictStateLockOperation {
		s.before()
	}
	return s.Store.Update(ctx, key, operation, fn)
}

func TestApplyVerdictProjectsCommittedConcurrentFinding(t *testing.T) {
	root, server := setupMergeReviewStatusRun(t, apiv1.Verdict{Decision: apiv1.VerdictPass}, "reviewed-head", "reviewed-base")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	open := openStageStateStore
	var inserted atomic.Bool
	openStageStateStore = func(layout instance.Layout) (stateclient.Store, error) {
		store, err := open(layout)
		return interposedVerdictStore{Store: store, before: func() {
			if !inserted.CompareAndSwap(false, true) {
				return
			}
			// This writer arrives after the stage's legacy read and before its
			// commit, reproducing the previously lost concurrent finding.
			if err := updateVerdictRecord(t.Context(), store, verdictRecordKey(repo, 10), func(apiv1.Verdict, bool) (apiv1.Verdict, bool, error) {
				return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, HeadSHA: "reviewed-head", BaseSHA: "reviewed-base", LedgerGeneration: 1, Findings: []apiv1.Finding{{ID: "concurrent", LedgerGeneration: 1, Class: apiv1.FindingSubstantive, Severity: apiv1.SeverityError, Message: "Concurrent defect."}}}, true, nil
			}); err != nil {
				t.Error(err)
			}
		}}, err
	}
	t.Cleanup(func() { openStageStateStore = open })
	code, stdout, stderr := runArgs(t, "apply-verdict", root)
	if code != 0 {
		t.Fatalf("apply-verdict: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	statuses := server.postedStatuses()
	if len(statuses) != 1 || statuses[0].state != "failure" {
		t.Fatalf("stale pass status published: %+v", statuses)
	}
	comments, _ := fakeIssueComments(t, server, 10)
	if len(comments) != 1 {
		t.Fatalf("comments = %v", comments)
	}
	posted, ok := parseVerdictComment(comments[0])
	if !ok || posted.Decision != apiv1.VerdictNeedsChanges || posted.LedgerGeneration != 2 || len(posted.StandingFindings) != 1 {
		t.Fatalf("comment differs from committed verdict: %+v", posted)
	}
	if !issueHasLabel(server, 10, needsRemediationLabel) || readVerdictResult(t, "verdict-result.json")["decision"] != "needs-changes" {
		t.Fatal("labels or stage result retained pre-commit pass")
	}
	server.mu.Lock()
	reviews := append([]fakeReview(nil), server.prs[10].reviews...)
	server.mu.Unlock()
	if len(reviews) != 1 || reviews[0].state != "CHANGES_REQUESTED" || !strings.Contains(reviews[0].body, `"ledgerGeneration":2`) {
		t.Fatalf("native review retained pre-commit pass: %+v", reviews)
	}
}

// Before the first committed generation, an absent or legacy KV record cannot
// stand in for the complete thread history. Publishing a snapshot on a read
// failure would make the missed generation-zero findings invisible forever.
func TestADOReviewedVerdictFailsClosedWhenLegacyEvidenceIsUnreadable(t *testing.T) {
	for _, legacyState := range []bool{false, true} {
		for _, endpoint := range []string{"connectionData", "threads"} {
			t.Run(fmt.Sprintf("legacy-state=%t/%s", legacyState, endpoint), func(t *testing.T) {
				root := newVerdictTestRoot(t)
				repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}
				if legacyState {
					if err := writeVerdictState(root, repo, 55, apiv1.Verdict{Decision: apiv1.VerdictPass, HeadSHA: "head", BaseSHA: "base"}); err != nil {
						t.Fatal(err)
					}
				}
				publication, err := beginVerdictPublication(root, repo, 55)
				if err != nil {
					t.Fatal(err)
				}
				var writes atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method != http.MethodGet:
						writes.Add(1)
						http.Error(w, "unexpected publication", http.StatusBadRequest)
					case strings.HasSuffix(r.URL.Path, "/"+endpoint):
						http.Error(w, "unreadable evidence", http.StatusForbidden)
					case strings.HasSuffix(r.URL.Path, "/connectionData"):
						_, _ = io.WriteString(w, `{"authenticatedUser":{"id":"reviewer","providerDisplayName":"reviewer"}}`)
					default:
						_, _ = io.WriteString(w, `{"value":[]}`)
					}
				}))
				defer server.Close()
				provider := providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL })
				var stdout, stderr bytes.Buffer
				code := publishADOReviewedVerdict(t.Context(), root, provider, repo, 55, providers.PullRequestSummary{HeadSHA: "head", BaseSHA: "base"}, apiv1.Verdict{Decision: apiv1.VerdictPass}, publication, filepath.Join(root, "result.json"), &stdout, &stderr)
				current, err := publication.store.Get(t.Context(), verdictStateKey(publication.key))
				if err != nil {
					t.Fatal(err)
				}
				if code == 0 || writes.Load() != 0 || current.ETag != publication.observed.ETag {
					t.Fatalf("unreadable legacy evidence published a snapshot: code=%d writes=%d before=%s after=%s stderr=%s", code, writes.Load(), publication.observed.ETag, current.ETag, stderr.String())
				}
			})
		}
	}
}
