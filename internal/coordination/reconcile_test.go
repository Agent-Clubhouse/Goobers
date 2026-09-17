package coordination_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/coordination"
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/providers"
)

type fakeProvider struct {
	items                 map[string]providers.WorkItem
	pr                    providers.PullRequestSummary
	release               providers.ReleaseObservation
	creates               int
	failCreateAfterWrite  bool
	failCreateBeforeWrite bool
	readError             error
	dropBody              bool
	dropLabels            bool
}

func (f *fakeProvider) GetWorkItem(_ context.Context, _ providers.RepositoryRef, id string) (providers.WorkItem, error) {
	if f.readError != nil {
		return providers.WorkItem{}, f.readError
	}
	item, ok := f.items[id]
	if !ok {
		return item, fmt.Errorf("missing issue %s", id)
	}
	return item, nil
}
func (f *fakeProvider) FindWorkItemsByMarker(_ context.Context, _ providers.RepositoryRef, marker string) ([]providers.WorkItem, error) {
	if f.readError != nil {
		return nil, f.readError
	}
	var items []providers.WorkItem
	for _, item := range f.items {
		if strings.Contains(item.Body, marker) {
			items = append(items, item)
		}
	}
	return items, nil
}
func (f *fakeProvider) CreateWorkItem(_ context.Context, req providers.CreateWorkItemRequest) (providers.WorkItem, error) {
	f.creates++
	if f.failCreateBeforeWrite {
		return providers.WorkItem{}, errors.New("create outcome unknown")
	}
	item := providers.WorkItem{ID: strconv.Itoa(6 + f.creates), Revision: "1", Title: req.Title, Body: req.Body, Labels: req.Labels, State: "open", Assignee: req.Assignee}
	f.items[item.ID] = item
	if f.failCreateAfterWrite {
		f.failCreateAfterWrite = false
		return providers.WorkItem{}, errors.New("response lost after committed create")
	}
	return item, nil
}
func (f *fakeProvider) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	item := f.items[req.ID]
	if req.ExpectedRevision == "" || req.ExpectedRevision != item.Revision {
		return item, errors.New("revision conflict")
	}
	item.Revision += "x"
	if req.Body != nil && !f.dropBody {
		item.Body = *req.Body
	}
	if req.State != "" {
		item.State = req.State
	}
	for _, label := range req.RemoveLabels {
		item.Labels = slices.DeleteFunc(item.Labels, func(s string) bool { return s == label })
	}
	for _, label := range req.AddLabels {
		if !f.dropLabels && !item.HasLabel(label) {
			item.Labels = append(item.Labels, label)
		}
	}
	f.items[req.ID] = item
	return item, nil
}
func (f *fakeProvider) GetPullRequest(context.Context, providers.RepositoryRef, string) (providers.PullRequestSummary, error) {
	return f.pr, f.readError
}
func (f *fakeProvider) GetCoordinationRelease(context.Context, providers.RepositoryRef, string, string) (providers.ReleaseObservation, error) {
	return f.release, f.readError
}

func fixture(t *testing.T) (coordination.Plan, coordination.Reconciler, []*fakeProvider) {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "..", "examples", "coordination", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var plan coordination.Plan
	if err := coordination.Decode(file, &plan); err != nil {
		t.Fatal(err)
	}
	digest, _ := coordination.Digest(plan)
	a := coordination.Authority{Name: plan.Gaggle, ParentRepo: plan.Parent.Repository, ApprovedPlans: map[string]string{plan.ID: digest}, ApprovedEvidence: map[string]string{}}
	all := []*fakeProvider{{items: map[string]providers.WorkItem{"42": {ID: "42", Revision: "1", State: "open", Body: "PRIVATE CONTEXT MUST NOT BE COPIED"}}}}
	r := coordination.Reconciler{Authority: a, Providers: map[string]coordination.Provider{plan.Parent.Repository.Key(): all[0]}, Leaser: decomposition.FileTargetLeaser{Directory: t.TempDir()}}
	for _, child := range plan.Children {
		f := &fakeProvider{items: map[string]providers.WorkItem{}}
		all = append(all, f)
		r.Providers[child.Repository.Key()] = f
		r.Authority.Targets = append(r.Authority.Targets, coordination.Target{Repository: child.Repository, Approval: "reviewed-plan"})
	}
	return plan, r, all
}

func reconcile(t *testing.T, r coordination.Reconciler, p coordination.Plan, e *coordination.Evidence) coordination.Result {
	t.Helper()
	out, err := r.Reconcile(context.Background(), p, e)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func approveEvidence(r *coordination.Reconciler, p coordination.Plan, e *coordination.Evidence) {
	digest, _ := coordination.Digest(e)
	r.Authority.ApprovedEvidence[p.ID] = digest
}
func mergedEvidence(p coordination.Plan, f []*fakeProvider) *coordination.Evidence {
	digest, _ := coordination.Digest(p)
	e := &coordination.Evidence{PlanDigest: digest}
	for i, child := range p.Children {
		head, merge := strings.Repeat(fmt.Sprint(i+1), 40), strings.Repeat(fmt.Sprint(i+3), 40)
		f[i+1].pr = providers.PullRequestSummary{ID: "9", HeadSHA: head, MergeSHA: merge, Merged: true, State: "closed"}
		b := coordination.Binding{NodeRef: child.NodeRef, Issue: "7", PR: "9", HeadSHA: head, MergeSHA: merge}
		if child.Completion == "release" {
			b.ReleaseSHA = strings.Repeat("a", 40)
			f[i+1].release = providers.ReleaseObservation{Tag: child.ReleaseTag, SHA: b.ReleaseSHA, Published: true, IncludesCommit: true}
		}
		e.Children = append(e.Children, b)
	}
	return e
}

func TestCoordinationExampleEndToEnd(t *testing.T) {
	t.Run("separate-owner", func(t *testing.T) { coordinationEndToEnd(t, false) })
	t.Run("coordinator-primary-child", func(t *testing.T) { coordinationEndToEnd(t, true) })
}

func coordinationEndToEnd(t *testing.T, primaryChild bool) {
	t.Helper()
	p, r, f := fixture(t)
	if primaryChild {
		p.Children[1].Repository = p.Parent.Repository
		r.Authority.Targets = append(r.Authority.Targets, coordination.Target{Repository: p.Parent.Repository, Approval: "reviewed-plan"})
		r.Authority.ApprovedPlans[p.ID], _ = coordination.Digest(p)
		f[2] = f[0]
	}
	out := reconcile(t, r, p, nil)
	if out.Children[0].State != "ready" || out.Children[1].State != "blocked" {
		t.Fatalf("%+v", out)
	}
	if f[2].items["7"].HasLabel(providers.LabelApproved) {
		t.Fatal("downstream approved before dependency completion")
	}
	if !f[0].items["42"].HasLabel(providers.LabelCoordinationWait) || !f[2].items["7"].HasLabel(providers.LabelCoordinationWait) || f[1].items["7"].HasLabel(providers.LabelCoordinationWait) {
		t.Fatal("coordinator did not keep parent/downstream waiting and release only eligible child")
	}
	if !strings.Contains(f[2].items["7"].Body, "acme/core#7") {
		t.Fatal("cross-repo dependency link missing")
	}
	for _, provider := range f[1:] {
		if strings.Contains(provider.items["7"].Body, "PRIVATE") {
			t.Fatal("private context leaked")
		}
	}
	e := mergedEvidence(p, f)
	// Merge alone does not satisfy a required release.
	f[1].release.Published = false
	approveEvidence(&r, p, e)
	out = reconcile(t, r, p, e)
	if out.Children[0].State != "blocked" || out.Children[1].State != "blocked" {
		t.Fatalf("release bypass: %+v", out)
	}
	f[1].release.Published = true
	out = reconcile(t, r, p, e)
	if out.State != "integration-required" || f[0].items["42"].State != "open" {
		t.Fatalf("%+v", out)
	}
	r.ArtifactDigest = strings.Repeat("b", 64)
	e.Integration = &coordination.Integration{Command: p.IntegrationCommand, Passed: true, ArtifactSHA256: r.ArtifactDigest, Children: append([]coordination.Binding(nil), e.Children...)}
	approveEvidence(&r, p, e)
	out = reconcile(t, r, p, e)
	if out.State != "complete" || f[0].items["42"].State != "closed" {
		t.Fatalf("%+v", out)
	}
	// A changed live head invalidates the pinned evidence and reopens parent.
	f[2].pr.HeadSHA = strings.Repeat("c", 40)
	out = reconcile(t, r, p, e)
	if out.State == "complete" || f[0].items["42"].State != "open" {
		t.Fatal("stale integration accepted")
	}
}

func TestCoordinationCrashRetryAndConcurrentDuplicateSuppression(t *testing.T) {
	p, r, f := fixture(t)
	f[1].failCreateAfterWrite = true
	if _, err := r.Reconcile(context.Background(), p, nil); err == nil {
		t.Fatal("expected ambiguous create error")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := r.Reconcile(context.Background(), p, nil); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f[1].creates != 1 || f[2].creates != 1 {
		t.Fatalf("duplicate creates: %d/%d", f[1].creates, f[2].creates)
	}
}

func TestCoordinationRejectsInvalidAuthorityAndPlans(t *testing.T) {
	tests := map[string]func(*coordination.Plan, *coordination.Reconciler){
		"unapproved":                func(p *coordination.Plan, r *coordination.Reconciler) { r.Authority.ApprovedPlans = nil },
		"unknown target":            func(p *coordination.Plan, r *coordination.Reconciler) { p.Children[0].Repository.Name = "secret" },
		"missing explicit approval": func(p *coordination.Plan, r *coordination.Reconciler) { r.Authority.Targets[0].Approval = "" },
		"unknown dependency": func(p *coordination.Plan, r *coordination.Reconciler) {
			p.Children[1].DependsOn[0].Repository.Name = "consumer"
		},
		"cycle": func(p *coordination.Plan, r *coordination.Reconciler) {
			p.Children[0].DependsOn = []coordination.NodeRef{p.Children[1].NodeRef}
		},
		"closure is not code": func(p *coordination.Plan, r *coordination.Reconciler) { p.Children[0].Completion = "closed" },
		"enterprise": func(p *coordination.Plan, r *coordination.Reconciler) {
			p.Children[0].Repository.BaseURL = "https://other.example"
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			p, r, f := fixture(t)
			change(&p, &r)
			if _, err := r.Reconcile(context.Background(), p, nil); err == nil {
				t.Fatal("accepted invalid plan")
			}
			if f[1].creates+f[2].creates != 0 {
				t.Fatal("provider writes before admission")
			}
		})
	}
}

func TestCoordinationFailureCancellationAndAbandonedPR(t *testing.T) {
	for _, mode := range []string{"provider", "cancelled", "abandoned", "closed-only", "head-moved"} {
		t.Run(mode, func(t *testing.T) {
			p, r, f := fixture(t)
			reconcile(t, r, p, nil)
			e := mergedEvidence(p, f)
			switch mode {
			case "provider":
				f[1].readError = errors.New("upstream unavailable")
			case "cancelled":
				item := f[1].items["7"]
				item.State, item.StateReason = "closed", "not_planned"
				f[1].items["7"] = item
			case "abandoned":
				f[1].pr.Merged = false
			case "closed-only":
				item := f[1].items["7"]
				item.State, item.StateReason = "closed", "completed"
				f[1].items["7"] = item
				e.Children = nil
			case "head-moved":
				f[1].pr.HeadSHA = strings.Repeat("d", 40)
			}
			approveEvidence(&r, p, e)
			out, err := r.Reconcile(context.Background(), p, e)
			if mode == "provider" && err == nil {
				t.Fatal("provider failure swallowed")
			}
			if out.State == "complete" || f[2].items["7"].HasLabel(providers.LabelReady) {
				t.Fatal("upstream failure released downstream")
			}
		})
	}
}

func TestCoordinationEvidenceAndPublicationTampering(t *testing.T) {
	p, r, f := fixture(t)
	reconcile(t, r, p, nil)
	e := mergedEvidence(p, f)
	if _, err := r.Reconcile(context.Background(), p, e); err == nil {
		t.Fatal("unapproved evidence accepted")
	}
	approveEvidence(&r, p, e)
	item := f[1].items["7"]
	item.Body += "\nUnreviewed instructions"
	f[1].items["7"] = item
	if _, err := r.Reconcile(context.Background(), p, e); err == nil {
		t.Fatal("unreviewed publication approved")
	}
}

func TestCoordinationIntegrationPinsAndArtifact(t *testing.T) {
	for _, mode := range []string{"wrong artifact", "wrong command", "failed", "missing child", "wrong repo", "wrong release"} {
		t.Run(mode, func(t *testing.T) {
			p, r, f := fixture(t)
			reconcile(t, r, p, nil)
			e := mergedEvidence(p, f)
			r.ArtifactDigest = strings.Repeat("b", 64)
			e.Integration = &coordination.Integration{Command: p.IntegrationCommand, Passed: true, ArtifactSHA256: r.ArtifactDigest, Children: append([]coordination.Binding(nil), e.Children...)}
			switch mode {
			case "wrong artifact":
				e.Integration.ArtifactSHA256 = strings.Repeat("c", 64)
			case "wrong command":
				e.Integration.Command = "echo success"
			case "failed":
				e.Integration.Passed = false
			case "missing child":
				e.Integration.Children = e.Integration.Children[:1]
			case "wrong repo":
				e.Integration.Children[0].Repository.Name = "consumer"
			case "wrong release":
				e.Integration.Children[0].ReleaseSHA = strings.Repeat("d", 40)
			}
			approveEvidence(&r, p, e)
			if out := reconcile(t, r, p, e); out.State != "integration-required" {
				t.Fatalf("%+v", out)
			}
		})
	}
}

func TestCoordinationStrictDecodeAndStableDigest(t *testing.T) {
	var p coordination.Plan
	for _, raw := range []string{`{"version":1,"typo":true}`, `{} {}`} {
		if err := coordination.Decode(strings.NewReader(raw), &p); err == nil {
			t.Fatal("non-strict JSON")
		}
		if err := coordination.Decode(strings.NewReader("{}"+strings.Repeat(" ", 2<<20)), &p); err == nil {
			t.Fatal("oversized document accepted")
		}
	}
	p, r, _ := fixture(t)
	digest, _ := coordination.Digest(p)
	if digest != r.Authority.ApprovedPlans[p.ID] {
		t.Fatal("unstable digest")
	}
	if !reflect.DeepEqual(p.Children[1].DependsOn[0], p.Children[0].NodeRef) {
		t.Fatal("example dependency not fully qualified")
	}
}

func TestCoordinationAmbiguousCreateRequiresManualRecovery(t *testing.T) {
	p, r, f := fixture(t)
	f[1].failCreateBeforeWrite = true
	if _, err := r.Reconcile(t.Context(), p, nil); err == nil {
		t.Fatal("expected ambiguous outcome")
	}
	if _, err := r.Reconcile(t.Context(), p, nil); err == nil || !strings.Contains(err.Error(), "prior create intent") {
		t.Fatalf("unsafe automatic retry: %v", err)
	}
	if f[1].creates != 1 {
		t.Fatal("repeated ambiguous POST")
	}
	f[1].failCreateBeforeWrite = false
	draft := coordination.Publications(p)[0]
	if _, err := f[1].CreateWorkItem(t.Context(), providers.CreateWorkItemRequest{Repository: draft.Repository.Ref(), Title: draft.Title, Body: draft.Body, Labels: draft.Labels, Assignee: draft.Assignee}); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, p, nil)
	if f[1].creates != 2 {
		t.Fatal("manual recovery issue was not adopted")
	}
}

func TestCoordinationDeletedMarkerCannotCreateDuplicate(t *testing.T) {
	p, r, f := fixture(t)
	reconcile(t, r, p, nil)
	item := f[1].items["7"]
	item.Body = "marker removed"
	f[1].items["7"] = item
	if _, err := r.Reconcile(t.Context(), p, nil); err == nil {
		t.Fatal("missing marker accepted")
	}
	if f[1].creates != 1 {
		t.Fatal("missing marker created duplicate issue")
	}
}

func TestCoordinationSerializesChildrenWithinRepository(t *testing.T) {
	p, r, f := fixture(t)
	p.Children[1].Repository = p.Children[0].Repository
	p.Children[1].DependsOn = nil
	r.Authority.Targets = r.Authority.Targets[:1]
	r.Authority.ApprovedPlans[p.ID], _ = coordination.Digest(p)
	out := reconcile(t, r, p, nil)
	if out.Children[0].State != "ready" || out.Children[1].State != "blocked" || f[1].items["8"].HasLabel(providers.LabelReady) {
		t.Fatalf("parallel child release: %+v", out)
	}
	e := mergedEvidence(p, f)
	e.Children = e.Children[:1]
	approveEvidence(&r, p, e)
	out = reconcile(t, r, p, e)
	if out.Children[1].State != "ready" || !f[1].items["8"].HasLabel(providers.LabelReady) {
		t.Fatalf("next child not released: %+v", out)
	}
}

func TestCoordinationDependencyReleaseUsesMergedAndReleasedNotClosed(t *testing.T) {
	p, r, f := fixture(t)
	reconcile(t, r, p, nil)
	e := mergedEvidence(p, f)
	e.Children = e.Children[:1]
	approveEvidence(&r, p, e)
	out := reconcile(t, r, p, e)
	if out.Children[0].IssueClosed || !out.Children[0].PRMerged || out.Children[1].State != "ready" {
		t.Fatalf("incorrect condition semantics: %+v", out)
	}
	f[1].release.SHA = strings.Repeat("f", 40)
	out = reconcile(t, r, p, e)
	if out.Children[1].State != "blocked" || f[2].items["7"].HasLabel(providers.LabelReady) {
		t.Fatal("release invalidation did not withdraw eligibility")
	}
}

func TestCoordinationParentPinConflictAndBounds(t *testing.T) {
	p, r, f := fixture(t)
	reconcile(t, r, p, nil)
	item := f[0].items["42"]
	item.Body += "\n<!-- goobers-coordination:plan:other -->"
	f[0].items["42"] = item
	if _, err := r.Reconcile(t.Context(), p, nil); err == nil {
		t.Fatal("ambiguous parent plan pin accepted")
	}
	p, r, _ = fixture(t)
	p.Children = make([]coordination.Child, 101)
	if err := p.Validate(r.Authority, false); err == nil {
		t.Fatal("unbounded plan accepted")
	}
}

func TestCoordinationHonorsOwnerEscalationAndClaim(t *testing.T) {
	p, r, f := fixture(t)
	reconcile(t, r, p, nil)
	item := f[1].items["7"]
	item.Labels = append(item.Labels, providers.LabelClaimed)
	f[1].items["7"] = item
	out := reconcile(t, r, p, nil)
	if out.Children[0].State != "in-progress" || f[1].items["7"].HasLabel(providers.LabelReady) {
		t.Fatalf("claimed work was released again: %+v", out)
	}
	item = f[1].items["7"]
	item.Status = providers.WorkItemStatusInReview
	f[1].items["7"] = item
	out = reconcile(t, r, p, nil)
	if out.Children[0].State != "in-review" || f[1].items["7"].HasLabel(providers.LabelReady) {
		t.Fatal("unbound PR review was released as fresh implementation")
	}
	item = f[1].items["7"]
	item.Labels = append(item.Labels, providers.LabelNeedsHuman)
	f[1].items["7"] = item
	out = reconcile(t, r, p, nil)
	if out.Children[0].State != "blocked" || !f[1].items["7"].HasLabel(providers.LabelNeedsHuman) {
		t.Fatal("owner escalation was silently cleared")
	}
}

func TestCoordinationTaskClosureRequiresCompletedReason(t *testing.T) {
	p, r, f := fixture(t)
	p.Children[0].Kind, p.Children[0].Completion, p.Children[0].ReleaseTag = "task", "closed", ""
	r.Authority.ApprovedPlans[p.ID], _ = coordination.Digest(p)
	reconcile(t, r, p, nil)
	item := f[1].items["7"]
	item.State = "closed"
	f[1].items["7"] = item
	if out := reconcile(t, r, p, nil); out.Children[0].State != "blocked" {
		t.Fatal("unknown closure reason accepted")
	}
	item = f[1].items["7"]
	item.StateReason = "completed"
	f[1].items["7"] = item
	if out := reconcile(t, r, p, nil); out.Children[0].State != "complete" || out.Children[1].State != "ready" {
		t.Fatalf("completed non-code task did not release consumer: %+v", out)
	}
}

func TestCoordinationVerifiesProviderMutationResults(t *testing.T) {
	for _, mode := range []string{"body", "labels"} {
		t.Run(mode, func(t *testing.T) {
			p, r, f := fixture(t)
			if mode == "body" {
				f[0].dropBody = true
			} else {
				f[1].dropLabels = true
			}
			out, err := r.Reconcile(t.Context(), p, nil)
			if err == nil || out.State == "complete" {
				t.Fatal("silently ignored mutation reported success")
			}
			if f[1].items["7"].HasLabel(providers.LabelReady) {
				t.Fatal("ignored label update reported eligibility")
			}
			if mode == "body" && f[1].creates != 0 {
				t.Fatal("publication started without persisted parent pin")
			}
		})
	}
}

func TestCoordinationReviewedAssigneeRoutesChild(t *testing.T) {
	p, r, f := fixture(t)
	p.Children[0].Assignee = "implementer"
	if err := p.Validate(r.Authority, true); err == nil {
		t.Fatal("assignee changed without new plan approval")
	}

	r.Authority.ApprovedPlans[p.ID], _ = coordination.Digest(p)
	reconcile(t, r, p, nil)
	if f[1].items["7"].Assignee != "implementer" || coordination.Publications(p)[0].Assignee != "implementer" {
		t.Fatal("reviewed assignee missing from publication")
	}
	item := f[1].items["7"]
	item.Assignee = "different-user"
	f[1].items["7"] = item
	if _, err := r.Reconcile(t.Context(), p, nil); err == nil {
		t.Fatal("unreviewed reassignment accepted")
	}
}

func TestCoordinationRejectsParentAsPublishedChild(t *testing.T) {
	p, r, f := fixture(t)
	p.Children = p.Children[:1]
	p.Children[0].Repository = p.Parent.Repository
	r.Authority.Targets = []coordination.Target{{Repository: p.Parent.Repository, Approval: "reviewed-plan"}}
	r.Authority.ApprovedPlans[p.ID], _ = coordination.Digest(p)
	draft := coordination.Publications(p)[0]
	parent := f[0].items[p.Parent.ID]
	parent.Body = draft.Body
	f[0].items[parent.ID] = parent
	if _, err := r.Reconcile(t.Context(), p, nil); err == nil || !strings.Contains(err.Error(), "parent issue cannot") {
		t.Fatalf("parent adopted as child: %v", err)
	}
	if f[0].creates != 0 {
		t.Fatal("parent collision created work")
	}
}
