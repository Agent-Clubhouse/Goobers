package main

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

type routingProbe struct{ invokes, reviews int }

func (p *routingProbe) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	p.invokes++
	return apiv1.ResultEnvelope{}, nil
}
func (p *routingProbe) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	p.reviews++
	return apiv1.Verdict{}, nil
}
func (*routingProbe) HasAssetBundle() bool { return true }

func TestParentRoutingRequiresTransportAndPinnedTask(t *testing.T) {
	f := containedParentFixture(t)
	run, env := configuredChildStage(t, f)
	ordinary := &routingProbe{}
	routed, err := routeContainedParent(f.layout.Root, env.Goober, run, ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = routed.Invoke(t.Context(), env); !errors.Is(err, childworkflow.ErrAuthorityUnavailable) {
		t.Fatal("missing worker transport did not refuse", err)
	}
	missing := env
	missing.ChildWorkflowOrigin = nil
	if _, err = routed.Invoke(t.Context(), missing); err == nil {
		t.Fatal("missing origin fell back to ordinary executor")
	}
	forged := env
	forged.TaskID = env.RunID + ":ordinary"
	if _, err = routed.Invoke(t.Context(), forged); err == nil {
		t.Fatal("unselected task gained parent execution")
	}
	if _, err = routed.Review(t.Context(), env); err == nil {
		t.Fatal("parent invoked as reviewer")
	}
	if ordinary.invokes != 0 || ordinary.reviews != 0 {
		t.Fatal("refused parent executed ordinary harness")
	}
	plain := forged
	plain.ChildWorkflowOrigin = nil
	if _, err = routed.Invoke(t.Context(), plain); err != nil {
		t.Fatal(err)
	}
	if _, err = routed.Review(t.Context(), plain); err != nil {
		t.Fatal(err)
	}
	if ordinary.invokes != 1 || ordinary.reviews != 1 {
		t.Fatal("ordinary shared profile lost routing", ordinary)
	}
	if !routed.(interface{ HasAssetBundle() bool }).HasAssetBundle() {
		t.Fatal("ordinary asset protection lost")
	}
	wrong, err := routeContainedParent(f.layout.Root, "other-profile", run, ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wrong.Invoke(t.Context(), env); err == nil || ordinary.invokes != 1 {
		t.Fatal("wrong profile fell back locally", err)
	}
}

func TestParentRoutingKeepsExactBranchRecorder(t *testing.T) {
	f := containedParentFixture(t)
	run, env := configuredChildStage(t, f)
	recorder, err := runner.OwnedBranchRecorder(run, 7)
	if err != nil {
		t.Fatal(err)
	}
	routed, err := routeContainedParent(f.layout.Root, env.Goober, recorder, &routingProbe{})
	if err != nil {
		t.Fatal(err)
	}
	value, ok := routed.(*parentRoutedGoober)
	if !ok {
		t.Fatal("opted-in workflow lacked router")
	}
	_, branch, err := runner.OwnedJournalScope(value.rec)
	if err != nil || branch != 7 {
		t.Fatal("parent factory changed physical branch", branch, err)
	}
	reader, err := journal.OpenReadOnly(value.rec.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil || id.RunID != env.RunID {
		t.Fatal("parent factory changed owner", id, err)
	}
}

func TestParentRoutingOpaqueRecorderNeverAcquiresParentAuthority(t *testing.T) {
	ordinary := &routingProbe{}
	routed, err := routeContainedParent(t.TempDir(), "coder", runnerWiringHarnessRecorder{dir: t.TempDir()}, ordinary)
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{TaskID: "unowned:plan", ChildWorkflowOrigin: &apiv1.ChildWorkflowOrigin{}}
	if _, err := routed.Invoke(t.Context(), env); err == nil || ordinary.invokes != 0 {
		t.Fatal("opaque recorder gained parent authority", err)
	}
	env.ChildWorkflowOrigin = nil
	if _, err := routed.Invoke(t.Context(), env); err != nil || ordinary.invokes != 1 {
		t.Fatal("ordinary adapter lost compatibility", err)
	}
}
