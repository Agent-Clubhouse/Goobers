package interactiveaccess

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestExecutionLeasePolicyPublicationWaitsForJoin(t *testing.T) {
	g := testGaggle()
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "run.restartStage")
	s, _ := testService(t, g, testSources())
	lease, err := s.BeginExecution(t.Context(), testPrincipal(), g.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	if lease.Context().Err() != nil {
		t.Fatal("unchanged policy canceled execution")
	}
	next := g.DeepCopy()
	next.Spec.InteractiveAccess = nil
	published := make(chan struct{})
	applied := make(chan error, 1)
	go func() { applied <- s.Apply([]apiv1.Gaggle{*next}, func() error { close(published); return nil }) }()
	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("reload did not cancel")
	}
	select {
	case <-published:
		t.Fatal("published before join")
	default:
	}
	if _, err := lease.Credential(context.Background(), "repository.read", Target{Kind: "repository", Repository: repositoryIdentity(g.Spec.Project)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("credential after cancellation: %v", err)
	}
	lease.Close()
	lease.Close()
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginExecution(t.Context(), testPrincipal(), g.Name); !errors.Is(err, ErrDenied) {
		t.Fatalf("new execution after revocation: %v", err)
	}
}

func TestExecutionLeaseUnjoinedRefusesPublicationAndPreservesPolicy(t *testing.T) {
	g := testGaggle()
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "run.restartStage")
	s, _ := testService(t, g, testSources())
	s.executionDrainTimeout = time.Millisecond
	lease, err := s.BeginExecution(t.Context(), testPrincipal(), g.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	changed := g.DeepCopy()
	changed.Spec.InteractiveAccess = nil
	called := false
	if err := s.Apply([]apiv1.Gaggle{*changed}, func() error { called = true; return nil }); !errors.Is(err, ErrExecutionNotJoined) {
		t.Fatalf("err=%v", err)
	}
	if called {
		t.Fatal("published while canceled execution unjoined")
	}
	if err := s.Authorize(testPrincipal(), g.Name, "run.restartStage"); err != nil {
		t.Fatal("failed publication changed policy")
	}
}

func TestExecutionLeaseUsesSelectedSourcesWithoutAutomationFallback(t *testing.T) {
	t.Setenv("HUMAN_CODE_TOKEN", "human-code-value")
	t.Setenv("HUMAN_ISSUES_TOKEN", "human-backlog-value")
	g := testGaggle()
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "run.restartStage")
	s, reg := testService(t, g, testSources())
	lease, err := s.BeginExecution(t.Context(), testPrincipal(), g.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	repo, err := lease.Credential(t.Context(), "repository.read", Target{Kind: "repository", Repository: repositoryIdentity(g.Spec.Project)})
	if err != nil || repo.Value != "human-code-value" {
		t.Fatalf("repo err=%v", err)
	}
	backlog, err := lease.Credential(t.Context(), "backlog.read", Target{Kind: "backlog"})
	if err != nil || backlog.Value != "human-backlog-value" {
		t.Fatalf("backlog err=%v", err)
	}
	if len(reg.values) != 2 {
		t.Fatal("credentials not registered")
	}
	if _, err := lease.Credential(t.Context(), "pr.repair", Target{Kind: "repository", Repository: repositoryIdentity(g.Spec.Project)}); !errors.Is(err, ErrDenied) {
		t.Fatalf("ungranted effect: %v", err)
	}
	wrong := g.Spec.Project
	wrong.Name = "other"
	if _, err := lease.Credential(t.Context(), "repository.read", Target{Kind: "repository", Repository: repositoryIdentity(wrong)}); err == nil {
		t.Fatal("cross repository credential")
	}
}

func TestExecutionLeaseUncertainWriterCannotBeClearedByClose(t *testing.T) {
	g := testGaggle()
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "run.restartStage")
	s, _ := testService(t, g, testSources())
	s.executionDrainTimeout = time.Millisecond
	lease, err := s.BeginExecution(t.Context(), testPrincipal(), g.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.CloseAfter(errors.New("writer termination unobservable")); !errors.Is(err, ErrExecutionNotJoined) {
		t.Fatal(err)
	}
	lease.Close()
	next := g.DeepCopy()
	next.Spec.InteractiveAccess = nil
	if err := s.Apply([]apiv1.Gaggle{*next}, nil); !errors.Is(err, ErrExecutionNotJoined) {
		t.Fatalf("failed writer proof was cleared: %v", err)
	}
}
