package childpublication

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

var (
	_ EffectObserver = (*providers.GitHubProvider)(nil)
	_ EffectObserver = (*providers.ADOProvider)(nil)
)

type publicationObserver struct {
	target                   Target
	sha                      string
	missingBranch, missingPR bool
	branchReads, prReads     atomic.Int32
}

func (o *publicationObserver) GetBranch(_ context.Context, repo providers.RepositoryRef, head string) (providers.BranchSummary, bool, error) {
	o.branchReads.Add(1)
	if repo != o.target.Repository || head != o.target.Head {
		return providers.BranchSummary{}, false, errors.New("observation target changed")
	}
	return providers.BranchSummary{Name: head, SHA: o.sha}, !o.missingBranch, nil
}
func (o *publicationObserver) FindPullRequestByBranch(_ context.Context, repo providers.RepositoryRef, head, base string) (providers.PullRequestResult, bool, error) {
	o.prReads.Add(1)
	if repo != o.target.Repository || head != o.target.Head || base != o.target.Base {
		return providers.PullRequestResult{}, false, errors.New("observation target changed")
	}
	return providers.PullRequestResult{ID: "7", Number: 7, URL: "https://github.com/acme/web/pull/7"}, !o.missingPR, nil
}
func pendingPRObservation(t *testing.T, q *triggerqueue.Store, target Target) (triggerqueue.ChildPublication, string) {
	t.Helper()
	sha := confirmedPublicationBranch(t, q, target)
	branch, err := q.ChildPublication(t.Context(), target.Child.Identity, "branch")
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := json.Marshal(PRIntent{Version: 1, BranchDigest: branch.Digest, Request: providers.PullRequestRequest{Repository: target.Repository, Head: target.Head, Base: target.Base, RunID: target.Child.RunID, Title: "PRIVATE INTENT TITLE", Body: "PRIVATE INTENT BODY"}})
	p, err := q.PrepareChildPublication(t.Context(), target.Child.Identity, "pr", intent)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.BeginChildPublicationEffect(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	return p, sha
}
func TestPublicationObservationSettlesLateEffectAfterTerminalCancellation(t *testing.T) {
	q, target, _ := publicationFixture(t)
	pending, sha := pendingPRObservation(t, q, target)
	at := target.Child.AcceptedAt.Add(time.Hour)
	if err := q.SetChildState(t.Context(), target.Child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: "immutable-result"}, at); err != nil {
		t.Fatal(err)
	}
	if err := q.AcknowledgeChild(t.Context(), target.Child.Identity, "immutable-result", at); err != nil {
		t.Fatal(err)
	}
	if err := q.FenceChildParent(t.Context(), target.Child.Identity.ChildParent, "human", at); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkChildParentSettled(t.Context(), target.Child.Identity.ChildParent, at); err != nil {
		t.Fatal(err)
	}
	before, err := q.GetChild(t.Context(), target.Child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := Inspect(t.Context(), q, target.Child.Identity)
	if err != nil || len(projected) != 2 || !projected[1].NeedsHuman {
		t.Fatal(projected, err)
	}
	raw, err := json.Marshal(projected)
	if err != nil || strings.Contains(string(raw), "PRIVATE INTENT") {
		t.Fatal("projection leaked authored text", string(raw), err)
	}
	destination, err := InspectTarget(t.Context(), q, target.Child.Identity, ActionPR, pending.Digest)
	if err != nil || destination.Repository != target.Repository || destination.ChildRunID != target.Child.RunID || destination.ParentRunID != target.Child.Identity.ParentRunID {
		t.Fatal(destination, err)
	}
	observer := &publicationObserver{target: target, sha: sha, missingPR: true}
	service := Reconciler{Queue: q, Observer: observer}
	notSeen, err := service.Check(t.Context(), target.Child.Identity, ActionPR, pending.Digest)
	if err != nil || notSeen.Observation != "not_observed" || !notSeen.NeedsHuman || notSeen.CheckedAt.IsZero() {
		t.Fatal(notSeen, err)
	}
	if pruned, err := q.PruneChildren(t.Context(), at.Add(3*triggerqueue.ChildRetention), 100); err != nil || pruned.Tombstoned != 0 {
		t.Fatal("pending effect lost", pruned, err)
	}
	observer.missingPR = false
	settled, err := service.Check(t.Context(), target.Child.Identity, ActionPR, pending.Digest)
	if err != nil || settled.NeedsHuman || settled.State != "confirmed" || settled.PullRequestNumber != 7 || settled.CreatedAt.IsZero() || settled.UpdatedAt.Before(settled.CreatedAt) || settled.CheckedAt.IsZero() {
		t.Fatal(settled, err)
	}
	after, err := q.GetChild(t.Context(), target.Child.Identity)
	if err != nil || after != before {
		t.Fatal("observation mutated child execution", after, err)
	}
	if err = q.BeginChildPublicationEffect(t.Context(), pending); !errors.Is(err, triggerqueue.ErrParentCancelled) {
		t.Fatal("new effect permitted after observation", err)
	}
	if _, err = service.Check(t.Context(), target.Child.Identity, ActionPR, pending.Digest); err != nil {
		t.Fatal(err)
	}
	if observer.branchReads.Load() != 2 || observer.prReads.Load() != 2 {
		t.Fatal("confirmed status repeated provider reads")
	}
	if pruned, err := q.PruneChildren(t.Context(), at.Add(3*triggerqueue.ChildRetention), 100); err != nil || pruned.Tombstoned != 1 {
		t.Fatal("observed effect stayed pinned", pruned, err)
	}
}
func TestPublicationObservationRefusesDigestScopeAndDrift(t *testing.T) {
	q, target, _ := publicationFixture(t)
	pending, sha := pendingPRObservation(t, q, target)
	observer := &publicationObserver{target: target, sha: sha}
	service := Reconciler{Queue: q, Observer: observer}
	if _, err := service.Check(t.Context(), target.Child.Identity, ActionPR, "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatal(err)
	}
	foreign := target.Child.Identity
	foreign.Gaggle = "foreign"
	if _, err := service.Check(t.Context(), foreign, ActionPR, pending.Digest); err == nil {
		t.Fatal("foreign lineage accepted")
	}
	if observer.branchReads.Load() != 0 {
		t.Fatal("invalid selectors reached provider")
	}
	observer.sha = strings.Repeat("c", 40)
	status, err := service.Check(t.Context(), target.Child.Identity, ActionPR, pending.Digest)
	if err != nil || status.Observation != "branch_changed" || !status.NeedsHuman || observer.prReads.Load() != 0 {
		t.Fatal(status, err)
	}
	record, err := q.ChildPublication(t.Context(), target.Child.Identity, "pr")
	if err != nil || record.State != "effect_pending" {
		t.Fatal(record, err)
	}
}
func TestPublicationObservationPreparedMeansNoEffectBegun(t *testing.T) {
	q, target, _ := publicationFixture(t)
	intent, _ := json.Marshal(BranchIntent{Version: 1, RunID: target.Child.RunID, Lineage: *target.Identity.Child, Repository: target.Repository, Remote: target.Remote, Head: target.Head, Base: target.Base, Commit: strings.Repeat("b", 40)})
	prepared, err := q.PrepareChildPublication(t.Context(), target.Child.Identity, "branch", intent)
	if err != nil {
		t.Fatal(err)
	}
	observer := &publicationObserver{target: target}
	status, err := (Reconciler{Queue: q, Observer: observer}).Check(t.Context(), target.Child.Identity, ActionBranch, prepared.Digest)
	if err != nil || status.NeedsHuman || status.Observation != "no_effect_begun" || observer.branchReads.Load() != 0 {
		t.Fatal(status, err)
	}
	if _, err = q.ChildPublication(t.Context(), target.Child.Identity, "pr"); !errors.Is(err, triggerqueue.ErrChildPublicationPending) {
		t.Fatal("check invented another intent", err)
	}
}

func TestPublicationObservationConfirmsExactPendingBranchOnly(t *testing.T) {
	q, target, _ := publicationFixture(t)
	sha := strings.Repeat("b", 40)
	intent, _ := json.Marshal(BranchIntent{Version: 1, RunID: target.Child.RunID, Lineage: *target.Identity.Child, Repository: target.Repository, Remote: target.Remote, Head: target.Head, Base: target.Base, Commit: sha})
	pending, err := q.PrepareChildPublication(t.Context(), target.Child.Identity, "branch", intent)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.BeginChildPublicationEffect(t.Context(), pending); err != nil {
		t.Fatal(err)
	}
	observer := &publicationObserver{target: target, sha: sha, missingBranch: true}
	service := Reconciler{Queue: q, Observer: observer}
	missing, err := service.Check(t.Context(), target.Child.Identity, ActionBranch, pending.Digest)
	if err != nil || !missing.NeedsHuman || missing.Observation != "not_observed" {
		t.Fatal(missing, err)
	}
	observer.missingBranch = false
	found, err := service.Check(t.Context(), target.Child.Identity, ActionBranch, pending.Digest)
	if err != nil || found.NeedsHuman || found.State != "confirmed" || found.Commit != sha || observer.prReads.Load() != 0 {
		t.Fatal(found, err)
	}
}
