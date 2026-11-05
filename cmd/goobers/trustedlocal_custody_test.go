package main

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestTrustedLocalBlockedChildCancellationNeverLaunches(t *testing.T) {
	permit := make(chan struct{}, 1)
	permit <- struct{}{}
	// No source is available: a blocked or cancelled waiter must not even reach
	// source admission, much less start a process or touch an attempt checkout.
	child := &trustedLocalChild{permit: permit}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := child.Invoke(ctx, apiv1.InvocationEnvelope{}); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("child bypassed held execution permit: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled child remained blocked")
	}
	if len(permit) != 1 {
		t.Fatal("cancelled waiter released another attempt's permit")
	}
	<-permit
}

func TestTrustedLocalCrashRetainsWriterWithoutRemoteRecovery(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	digest, writer := publishChildPodContract(t, f, id, "check", 1)
	defer func() { _ = writer.Close() }()
	marker := journal.Event{Type: journal.EventRunnerAnnotation, Stage: "check", Attempt: 1, Runner: map[string]any{"kind": childPodWriterStarted, "contractDigest": digest, "backend": "trusted-local"}}
	if err := writer.Append(marker); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(writer.Dir())
	if err != nil {
		t.Fatal(err)
	}
	s := &daemonCredentialService{layout: f.launcher.layout, childQueue: f.service.queue}
	s.childPodRecovery = func(context.Context, *journal.Reader, string, childPodScope) error {
		t.Fatal("local custody used remote recovery")
		return nil
	}
	before := writer.Seq()
	if err := s.reconcileChildPodCustody(t.Context(), reader); !errors.Is(err, invoke.ErrChildCustodyPending) {
		t.Fatalf("unjoined writer admitted recovery: %v", err)
	}
	pending, _, err := s.pendingChildPodScopes(t.Context(), reader)
	if err != nil || len(pending) != 1 || writer.Seq() != before {
		t.Fatalf("crash evidence lost: pending=%v err=%v", pending, err)
	}
	marker.Runner["kind"] = childPodWriterJoined
	if err := writer.Append(marker); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileChildPodCustody(t.Context(), reader); err != nil {
		t.Fatalf("acknowledged writer still pending: %v", err)
	}
}
