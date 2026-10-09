package runner

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

func TestEnforceExpectedOutputs(t *testing.T) {
	shell := apiv1.Task{
		Name: "enable-auto-merge", Type: apiv1.TaskDeterministic,
		Run:             &apiv1.DeterministicRun{Command: []string{"true"}},
		Inputs:          map[string]string{"resultFile": "result.json"},
		ExpectedOutputs: []string{"prNumber", "merged"},
	}
	withKind := func(task apiv1.Task, kind string) apiv1.Task {
		task.Inputs = map[string]string{"kind": kind}
		return task
	}
	agentic := shell
	agentic.Type, agentic.Run, agentic.Goober = apiv1.TaskAgentic, nil, "coder"
	noResultFile := shell
	noResultFile.Inputs = nil
	dynamicKind := shell
	dynamicKind.InputsFrom = map[string]string{"kind": "plan.kind"}
	boundResultFile := noResultFile
	boundResultFile.InputsFrom = map[string]string{"resultFile": "plan.resultFile"}
	partial := map[string]any{"merged": "do-not-leak-this-value"}
	complete := map[string]any{"prNumber": "194", "merged": true}

	for _, tc := range []struct {
		name     string
		version  string
		task     apiv1.Task
		status   apiv1.ResultStatus
		outputs  map[string]any
		wantFail bool
		wantMsg  []string
	}{
		{name: "dsl 3.0 stays advisory", version: supportmatrix.V3DSLVersion, task: shell, status: apiv1.ResultSuccess, outputs: partial},
		{name: "dsl 2.0 stays advisory", version: supportmatrix.V2DSLVersion, task: shell, status: apiv1.ResultSuccess, outputs: partial},
		{name: "unpinned stays advisory", task: shell, status: apiv1.ResultSuccess, outputs: partial},
		{name: "dsl 3.1 complete outputs succeed", version: supportmatrix.V31DSLVersion, task: shell, status: apiv1.ResultSuccess, outputs: complete},
		{
			name: "dsl 3.1 missing key fails producer", version: supportmatrix.V31DSLVersion, task: shell,
			status: apiv1.ResultSuccess, outputs: partial, wantFail: true,
			wantMsg: []string{`"enable-auto-merge"`, `"result.json"`, `"prNumber"`},
		},
		{
			name: "later dsl inherits enforcement", version: "3.2", task: shell,
			status: apiv1.ResultSuccess, outputs: nil, wantFail: true,
			wantMsg: []string{`"result.json"`, `"prNumber" "merged"`},
		},
		{
			name: "dsl 3.1 without result file names the missing channel", version: supportmatrix.V31DSLVersion, task: noResultFile,
			status: apiv1.ResultSuccess, outputs: nil, wantFail: true,
			wantMsg: []string{"no inputs.resultFile", `"prNumber"`},
		},
		{
			name: "dsl 3.1 result file bound through inputsFrom", version: supportmatrix.V31DSLVersion, task: boundResultFile,
			status: apiv1.ResultSuccess, outputs: nil, wantFail: true,
			wantMsg: []string{`"inputsFrom.resultFile"`, `"prNumber"`},
		},
		{name: "no-work is not a contract breach", version: supportmatrix.V31DSLVersion, task: shell, status: apiv1.ResultNoWork},
		{name: "failure keeps its own diagnosis", version: supportmatrix.V31DSLVersion, task: shell, status: apiv1.ResultFailure},
		{name: "built-in ci-poll owns its outputs", version: supportmatrix.V31DSLVersion, task: withKind(shell, "ci-poll"), status: apiv1.ResultSuccess},
		{name: "runtime-bound kind may be built in", version: supportmatrix.V31DSLVersion, task: dynamicKind, status: apiv1.ResultSuccess},
		{name: "agentic stage owns its outputs", version: supportmatrix.V31DSLVersion, task: agentic, status: apiv1.ResultSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := apiv1.ResultEnvelope{Status: tc.status, Outputs: tc.outputs}
			got := EnforceExpectedOutputs(tc.version, tc.task, in)
			if !tc.wantFail {
				if got.Status != tc.status || got.Error != nil {
					t.Fatalf("result = %+v, want status %q unchanged", got, tc.status)
				}
				return
			}
			if got.Status != apiv1.ResultFailure || got.Error == nil || got.Error.Code != MissingExpectedOutputsCode || got.Error.Retryable {
				t.Fatalf("result = %+v (error %+v), want non-retryable %s failure", got, got.Error, MissingExpectedOutputsCode)
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(got.Error.Message, want) {
					t.Errorf("message %q does not name %s", got.Error.Message, want)
				}
			}
			if strings.Contains(got.Error.Message, "do-not-leak-this-value") {
				t.Fatalf("message leaks result file contents: %q", got.Error.Message)
			}
		})
	}
}

// End to end through the shell executor, reproducing #5175's live shape: a
// producer and an intermediary both write their declared result files with a
// UTF-8 BOM (Windows PowerShell 5.1's `Set-Content -Encoding utf8`), and the
// intermediary must hand prNumber on to its successor's inputsFrom. When the
// intermediary drops the key, DSL 3.1 fails it — the producer of the gap —
// rather than letting it succeed and the successor die on a missing input.
func TestDSL31ExpectedOutputsBOMIntermediaryHandoff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stage commands use POSIX shell syntax")
	}
	const bom = `\357\273\277`
	for _, tc := range []struct {
		name         string
		dslVersion   string
		intermediary string
		wantPhase    journal.RunPhase
		wantFailure  string
		wantFailCode string
		wantStartErr string
	}{
		{
			name:         "bom-prefixed handoff reaches the successor",
			dslVersion:   supportmatrix.V31DSLVersion,
			intermediary: `printf '` + bom + `{"prNumber":"%s"}\r\n' "$GOOBERS_INPUT_PRNUMBER" > result.json`,
			wantPhase:    journal.PhaseCompleted,
		},
		{
			name:         "missing declared key fails the intermediary",
			dslVersion:   supportmatrix.V31DSLVersion,
			intermediary: `printf '` + bom + `{"autoMerge":"enabled"}\r\n' > result.json`,
			wantPhase:    journal.PhaseFailed,
			wantFailure:  "enable-auto-merge",
			wantFailCode: MissingExpectedOutputsCode,
		},
		{
			name:         "dsl 3.0 keeps the advisory behaviour",
			dslVersion:   supportmatrix.V3DSLVersion,
			intermediary: `printf '` + bom + `{"autoMerge":"enabled"}\r\n' > result.json`,
			wantStartErr: `execute stage "ci-poll": task "ci-poll": inputsFrom "prNumber": upstream output "prNumber" not found`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
			resolver, err := credentials.NewResolver(nil)
			if err != nil {
				t.Fatal(err)
			}
			r, err := New(Config{
				NewDeterministic: func(rec ArtifactRecorder, reg SecretRegistrar) (invoke.Deterministic, error) {
					injector, err := credentials.NewInjector(resolver, nil, reg)
					if err != nil {
						return nil, err
					}
					return executor.NewShellExecutor(injector, rec)
				},
				Worktrees:    wtMgr,
				RunsDir:      runsDir,
				ScratchDir:   t.TempDir(),
				RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			stage := func(name, script, next string, inputsFrom map[string]string, expected ...string) apiv1.Task {
				return apiv1.Task{
					Name: name, Type: apiv1.TaskDeterministic, Goal: name,
					Run:             &apiv1.DeterministicRun{Command: []string{"sh", "-c", script}, Workspace: apiv1.WorkspaceScratch},
					Inputs:          map[string]string{"resultFile": "result.json"},
					InputsFrom:      inputsFrom,
					ExpectedOutputs: expected,
					Next:            next,
				}
			}
			machine, err := workflow.Compile(workflow.Definition{
				Name: "bom-handoff", Version: 1, DSLVersion: tc.dslVersion,
				Spec: apiv1.WorkflowSpec{
					Gaggle:   "acme-web",
					Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
					Start:    "open-pr",
					Tasks: []apiv1.Task{
						stage("open-pr", `printf '`+bom+`{"prNumber":"194"}\r\n' > result.json`, "enable-auto-merge", nil, "prNumber"),
						stage("enable-auto-merge", tc.intermediary, "ci-poll", map[string]string{"prNumber": "prNumber"}, "prNumber"),
						stage("ci-poll", `printf '{"polled":"%s"}' "$GOOBERS_INPUT_PRNUMBER" > result.json`, workflow.TerminalComplete,
							map[string]string{"prNumber": "prNumber"}, "polled"),
					},
				},
			}, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			runID := "bom-handoff-" + strings.ReplaceAll(tc.name, " ", "-")
			result, err := r.Start(context.Background(), StartInput{
				RunID: runID, Machine: machine, Gaggle: "acme-web",
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			})
			if tc.wantStartErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantStartErr) {
					t.Fatalf("Start error = %v, want the successor to fail on %q", err, tc.wantStartErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if result.Phase != tc.wantPhase {
				t.Fatalf("phase = %q (failure %s/%s: %s), want %q", result.Phase,
					result.FailureStage, result.FailureCode, result.FailureMessage, tc.wantPhase)
			}
			finished := map[string]journal.Event{}
			for _, event := range readJournalEvents(t, filepath.Join(runsDir, runID)) {
				if event.Type == journal.EventStageFinished {
					finished[event.Stage] = event
				}
			}
			if tc.wantPhase == journal.PhaseCompleted {
				if got := finished["ci-poll"].Outputs["polled"]; got != "194" {
					t.Fatalf("ci-poll outputs = %+v, want polled=194 handed through the BOM-prefixed intermediary", finished["ci-poll"].Outputs)
				}
				return
			}
			if result.FailureStage != tc.wantFailure {
				t.Fatalf("failure stage = %q (%s: %s), want %q", result.FailureStage, result.FailureCode, result.FailureMessage, tc.wantFailure)
			}
			if tc.wantFailCode == "" {
				return
			}
			event, ok := finished[tc.wantFailure]
			if !ok || event.Status != string(apiv1.ResultFailure) || event.Error == nil || event.Error.Code != tc.wantFailCode {
				t.Fatalf("%s stage.finished = %+v, want %s failure", tc.wantFailure, event, tc.wantFailCode)
			}
			if !strings.Contains(event.Error.Message, `"result.json"`) || !strings.Contains(event.Error.Message, `"prNumber"`) ||
				strings.Contains(event.Error.Message, "enabled") {
				t.Fatalf("diagnostic %q must name the result file and missing key without its contents", event.Error.Message)
			}
			if _, ran := finished["ci-poll"]; ran {
				t.Fatal("successor ran after its producer broke the expectedOutputs contract")
			}
		})
	}
}
