//go:build integration

package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationTLC(t *testing.T) {
	testdep.Require(t, "java")
	testdep.RequireEnv(t, "GOOBERS_TEST_TLC_JAR")
	testdep.RequireEnv(t, "GOOBERS_TEST_JAVA_HOME")
	root, err := os.MkdirTemp(".", ".tlc-integration-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	config := filepath.Join(root, "reviewed")
	candidate := filepath.Join(root, "candidate")
	scratch := filepath.Join(root, "scratch")
	for _, directory := range []string{config, candidate, scratch} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	example := filepath.Join("..", "..", "examples", "backprop-tlc")
	spec, err := os.ReadFile(filepath.Join(example, "Spec.tla"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(example, "Spec.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	java := filepath.Join("bin", "java")
	if runtime.GOOS == "windows" {
		java += ".exe"
	}
	files, err := snapshotTLCRuntime(os.Getenv("GOOBERS_TEST_JAVA_HOME"), filepath.Join(root, "runtime-pin"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := TLCManifest{
		Property: "finite-state-not-two", Invariant: "Safe", Specification: "Spec.tla", SpecDigest: tlcDigest(spec),
		Configuration: "Spec.cfg", ConfigDigest: tlcDigest(cfg), Jar: os.Getenv("GOOBERS_TEST_TLC_JAR"),
		Runtime: os.Getenv("GOOBERS_TEST_JAVA_HOME"), RuntimeDigest: tlcRuntimeDigest(files), Java: java, Assumptions: []string{},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"Spec.tla": spec, "Spec.cfg": cfg, "manifest.json": encoded} {
		if err := os.WriteFile(filepath.Join(config, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name, file, timeout string
		want                creditgraph.VerificationOutcome
	}{
		{"positive", "positive.json", "30s", creditgraph.VerificationProven},
		{"counterexample", "broken.json", "30s", creditgraph.VerificationCounterexample},
		{"timeout", "positive.json", "1ns", creditgraph.VerificationInconclusive},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(example, test.file))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(candidate, "model.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			shell, _ := newPortableTestExecutor(t, nil)
			jr, err := journal.Create(filepath.Join(root, "runs"), journal.RunIdentity{RunID: "tlc-" + test.name, Workflow: "verify"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = jr.Close() }()
			recording := &tlcIntegrationJournal{Run: jr, recorded: map[string][]byte{}}
			shell.Journal = recording
			shell.ConfigDirectory, shell.ScratchDir = config, scratch
			executor := &TLCExecutor{Shell: shell}
			env := apiv1.InvocationEnvelope{TaskID: "check", Attempt: 1, Workspace: candidate,
				Inputs: map[string]any{InputKind: KindTLC, InputTLCManifest: "manifest.json", InputTLCModel: "model.json", InputTimeout: test.timeout}}
			registry := NewKindRegistry()
			if err := registry.Register(KindShell, shell); err != nil {
				t.Fatal(err)
			}
			if err := registry.Register(KindTLC, executor); err != nil {
				t.Fatal(err)
			}
			dispatcher, err := NewTaskExecutor(registry)
			if err != nil {
				t.Fatal(err)
			}
			if err := recording.Append(journal.Event{Type: journal.EventStageStarted, Stage: "check", Attempt: 1}); err != nil {
				t.Fatal(err)
			}
			result, err := dispatcher.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{KindTLC}, Workspace: apiv1.WorkspaceRepo})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("stdout:\n%s\nstderr:\n%s", recording.recorded["check/stdout.log"], recording.recorded["check/stderr.log"])
			if result.Status != apiv1.ResultSuccess || result.Outputs["verificationOutcome"] != string(test.want) {
				t.Fatalf("result = %+v, want %s", result, test.want)
			}
			report, err := creditgraph.DecodeVerificationReport(recording.recorded["check/verification.json"])
			if err != nil || report.Outcome != test.want || report.Subject != tlcDigest(data) {
				t.Fatalf("report = %+v, %v", report, err)
			}
			if err := recording.Append(journal.Event{Type: journal.EventStageFinished, Stage: "check", Attempt: 1, Status: string(result.Status)}); err != nil {
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
			graph, err := creditgraph.Build(creditgraph.Input{RunID: "tlc-integration", Events: events})
			if err != nil || len(graph.Verifications) != 1 || graph.Verifications[0].Outcome != test.want {
				t.Fatalf("real checker evidence was not accepted: %+v, %v", graph, err)
			}
			t.Logf("%s: %s checker=%+v runtime=%s", test.name, report.Outcome, report.Checker, manifest.RuntimeDigest)
			t.Logf("report: %s", recording.recorded["check/verification.json"])
		})
	}
}

type tlcIntegrationJournal struct {
	*journal.Run
	recorded map[string][]byte
}

func (j *tlcIntegrationJournal) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	ref, err := j.Run.RecordArtifact(name, data)
	j.recorded[name] = append([]byte(nil), data...)
	return ref, err
}
