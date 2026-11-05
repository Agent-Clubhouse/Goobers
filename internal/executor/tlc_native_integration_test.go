//go:build integration

package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationTLCReviewedNative(t *testing.T) {
	testdep.Require(t, "java")
	testdep.RequireEnv(t, "GOOBERS_TEST_TLC_JAR")
	testdep.RequireEnv(t, "GOOBERS_TEST_JAVA_HOME")
	root := tlcTestDirectory(t)
	modelDirectory := filepath.Join("..", "..", "test", "formal", "recovery")
	spec, err := os.ReadFile(filepath.Join(modelDirectory, "TerminalRecovery.tla"))
	if err != nil {
		t.Fatal(err)
	}
	files, err := snapshotTLCRuntime(os.Getenv("GOOBERS_TEST_JAVA_HOME"), filepath.Join(root, "runtime-pin"))
	if err != nil {
		t.Fatal(err)
	}
	java := filepath.Join("bin", "java")
	if runtime.GOOS == "windows" {
		java += ".exe"
	}
	for _, test := range []struct {
		name, config, timeout string
		want                  creditgraph.VerificationOutcome
	}{
		{"good", "TerminalRecovery.cfg", "30s", creditgraph.VerificationProven},
		{"bad", "TerminalRecoveryBad.cfg", "30s", creditgraph.VerificationCounterexample},
		{"timeout", "TerminalRecovery.cfg", "1ns", creditgraph.VerificationInconclusive},
	} {
		t.Run(test.name, func(t *testing.T) {
			caseRoot := filepath.Join(root, test.name)
			reviewed, candidate, scratch := filepath.Join(caseRoot, "reviewed"), filepath.Join(caseRoot, "candidate"), filepath.Join(caseRoot, "scratch")
			for _, directory := range []string{reviewed, candidate, scratch} {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := os.ReadFile(filepath.Join(modelDirectory, test.config))
			if err != nil {
				t.Fatal(err)
			}
			manifest := TLCManifest{Mode: tlcReviewedNative, Module: "TerminalRecovery", Property: "terminal-replay-stable", Invariant: "TerminalReplayStable",
				Specification: "TerminalRecovery.tla", Configuration: "TerminalRecovery.cfg", SpecDigest: tlcDigest(spec), ConfigDigest: tlcDigest(cfg),
				Jar: os.Getenv("GOOBERS_TEST_TLC_JAR"), Runtime: os.Getenv("GOOBERS_TEST_JAVA_HOME"), RuntimeDigest: tlcRuntimeDigest(files), Java: java,
				Assumptions: []string{"four terminal outcomes and MaxCrashes=2; no operator reopen or Go refinement"}}
			encoded, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string][]byte{"TerminalRecovery.tla": spec, "TerminalRecovery.cfg": cfg, "manifest.json": encoded} {
				if err := os.WriteFile(filepath.Join(reviewed, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			runTLCNativeIntegration(t, caseRoot, reviewed, candidate, scratch, test.timeout, spec, test.want)
		})
	}
}

func runTLCNativeIntegration(t *testing.T, root, reviewed, candidate, scratch, timeout string, spec []byte, want creditgraph.VerificationOutcome) {
	t.Helper()
	shell, _ := newPortableTestExecutor(t, nil)
	jr, err := journal.Create(filepath.Join(root, "runs"), journal.RunIdentity{RunID: "native", Workflow: "verify"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jr.Close() }()
	recording := &tlcIntegrationJournal{Run: jr, recorded: map[string][]byte{}}
	shell.Journal, shell.ConfigDirectory, shell.ScratchDir = recording, reviewed, scratch
	registry := NewKindRegistry()
	for kind, executor := range map[string]KindExecutor{KindShell: shell, KindTLC: &TLCExecutor{Shell: shell}} {
		if err := registry.Register(kind, executor); err != nil {
			t.Fatal(err)
		}
	}
	dispatcher, err := NewTaskExecutor(registry)
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{TaskID: "check", Attempt: 1, Workspace: candidate,
		Inputs: map[string]any{InputKind: KindTLC, InputTLCManifest: "manifest.json", InputTimeout: timeout}}
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "check", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{KindTLC}, Workspace: apiv1.WorkspaceScratch})
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: "check", Attempt: 1, Status: string(result.Status)}); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(jr.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	graph, err := creditgraph.Build(creditgraph.Input{RunID: "native", Events: events})
	if err != nil || len(graph.Verifications) != 1 || graph.Verifications[0].Outcome != want {
		t.Fatalf("native evidence was not accepted: %+v, %v; stdout=%s", graph, err, recording.recorded["check/stdout.log"])
	}
	report := graph.Verifications[0]
	if report.Subject != tlcDigest(spec) || !strings.Contains(string(recording.recorded["check/context.json"]), `"adapter":"tlc-reviewed-native-v1"`) {
		t.Fatal("native report did not bind the actual source and adapter mode")
	}
	t.Logf("%s\nreport: %s", recording.recorded["check/stdout.log"], recording.recorded["check/verification.json"])
}
