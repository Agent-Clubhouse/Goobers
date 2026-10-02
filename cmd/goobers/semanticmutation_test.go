package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/mutationreceipt"
	"github.com/goobers/goobers/internal/mutationsidecar"
)

func TestSemanticMutationStageCaptureAndSurrender(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(journalclient.EnvEndpoint, "")
	t.Setenv(journalclient.EnvToken, "")
	identity, err := mutationreceipt.New("github", "https://api.example.test/repos/a/b", "comment", "issue/7", map[string]string{"body": "semantic original"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := sidecarMutationRecorder{kind: "issue"}
	actionCalls := 0
	action := func(context.Context, mutationreceipt.Receipt) error {
		actionCalls++
		facts, err := mutationsidecar.ReadHandoff(".")
		if err != nil {
			t.Fatal(err)
		}
		if len(facts) != 1 || facts[0].SemanticMutation.Phase != "intent" {
			t.Fatalf("missing durable pre-action intent: %#v", facts)
		}
		return nil
	}
	if err := mutationreceipt.Capture(context.Background(), recorder, "source", identity, action); err != nil {
		t.Fatal(err)
	}
	remote, err := podMutationReceipts()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(dispatcher.SurrenderedResult{Mutations: remote})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Mutations []engine.MutationFact `json:"mutations"`
	}
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if actionCalls != 1 || len(decoded.Mutations) != 2 {
		t.Fatalf("capture/surrender lost actions: %d %#v", actionCalls, decoded.Mutations)
	}
	for index, fact := range decoded.Mutations {
		if fact.SemanticMutation == nil || fact.SemanticMutation.Mutation != identity {
			t.Fatal("semantic receipt lost during stage transport")
		}
		wantPhase := "intent"
		if index == 1 {
			wantPhase = "completed"
		}
		if fact.SemanticMutation.Phase != wantPhase {
			t.Fatal("receipt phase changed")
		}
	}
	if decoded.Mutations[0].ReceiptID == decoded.Mutations[1].ReceiptID || decoded.Mutations[0].SemanticMutation.ID != decoded.Mutations[1].SemanticMutation.ID {
		t.Fatal("custody and invocation identities conflated")
	}
}

func TestSemanticMutationStageCrashLeavesDurableIntent(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(journalclient.EnvEndpoint, "")
	t.Setenv(journalclient.EnvToken, "")
	identity, _ := mutationreceipt.New("github", "https://api.example.test/repos/a/b", "comment", "issue/7", "original")
	err := mutationreceipt.Capture(context.Background(), sidecarMutationRecorder{kind: "issue"}, "source", identity, func(context.Context, mutationreceipt.Receipt) error {
		return errors.New("response lost after remote commit")
	})
	if err == nil {
		t.Fatal("expected uncertain response")
	}
	facts, err := mutationsidecar.ReadHandoff(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].SemanticMutation.Phase != "intent" {
		t.Fatal("crash evidence falsely completed")
	}
	// A filesystem failure must precede any public action.
	if err := os.Remove(mutationsSidecarFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mutationsSidecarFile, 0o700); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := mutationreceipt.Capture(context.Background(), sidecarMutationRecorder{kind: "issue"}, "source", identity, func(context.Context, mutationreceipt.Receipt) error { called = true; return nil }); err == nil || called {
		t.Fatalf("failed intent allowed dispatch: called=%v err=%v", called, err)
	}
}
