package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/handoffcheck"
)

func reportSchemaContext(t *testing.T) context.Context {
	t.Helper()
	schema, err := handoffcheck.Compile("schemas/report.schema.json", "", []byte(`{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return handoffcheck.WithPublicationSchemas(t.Context(), map[string]*handoffcheck.Schema{"report": schema})
}

func reportPublicationEnvelope(workspace string) apiv1.InvocationEnvelope {
	env := testEnvelope(workspace)
	env.Attempt = 1
	env.ArtifactPublication = &apiv1.ArtifactPublication{Stage: "produce", Visit: 3, Slots: []apiv1.ArtifactSlot{{Name: "report", MediaType: "application/json", SchemaPath: "schemas/report.schema.json"}}}
	env.Inputs = map[string]any{InputArtifactManifestFile: "manifest.json"}
	return env
}

// stageReport writes the staging manifest and payload; an empty payload
// models an agent that omitted its output entirely.
func stageReport(workspace, payload string) error {
	if payload == "" {
		return nil
	}
	data, err := json.Marshal(artifactset.Manifest{SchemaVersion: artifactset.SchemaVersion, Entries: []artifactset.ManifestEntry{{Name: "report", Path: "payload.json", MediaType: "application/json"}}})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(workspace, "manifest.json"), data, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workspace, "payload.json"), []byte(payload), 0o600)
}

// publicationRepairingAdapter models a subprocess adapter's single bounded
// repair turn against the real completion validator.
type publicationRepairingAdapter struct {
	FakeAdapter
	status         apiv1.ResultStatus
	first, repair  string
	repairPrompts  []string
	firstErr       error
	sideEffectRuns int
}

func (a *publicationRepairingAdapter) Run(_ context.Context, req RunRequest) (Outcome, error) {
	a.sideEffectRuns++
	if err := stageReport(req.Workspace, a.first); err != nil {
		return Outcome{}, err
	}
	completion := apiv1.ResultEnvelope{Status: a.status}
	if a.status == apiv1.ResultFailure {
		completion.Error = &apiv1.ErrorInfo{Code: "agent_failed", Message: "could not finish"}
	}
	if err := WriteCompletion(req.Workspace, req.CompletionPath, completion); err != nil {
		return Outcome{}, err
	}
	payload, err := readCompletion(req.Workspace, req.CompletionPath)
	err = validateCompletion(req, payload, err)
	a.firstErr = err
	if repairableCompletionError(err) {
		a.repairPrompts = append(a.repairPrompts, renderCompletionRepairPrompt(req, err))
		if err := stageReport(req.Workspace, a.repair); err != nil {
			return Outcome{}, err
		}
		payload, err = readCompletion(req.Workspace, req.CompletionPath)
		err = validateCompletion(req, payload, err)
	}
	return Outcome{Payload: payload}, err
}

func TestExecutorRepairsInvalidPublicationInSameSession(t *testing.T) {
	for _, tc := range []struct {
		name, first, repair string
		wantSuccess         bool
	}{
		{name: "invalid then corrected", first: `{"summary":3}`, repair: `{"summary":"fixed"}`, wantSuccess: true},
		{name: "omitted then published", first: "", repair: `{"summary":"fixed"}`, wantSuccess: true},
		{name: "malformed then corrected", first: `{"summary":`, repair: `{"summary":"fixed"}`, wantSuccess: true},
		{name: "repair exhausted", first: `{"summary":3}`, repair: `{"summary":4}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeRecorder{}
			adapter := &publicationRepairingAdapter{status: apiv1.ResultSuccess, first: tc.first, repair: tc.repair}
			result, err := newPostconditionExecutor(t, adapter, rec).Invoke(reportSchemaContext(t), reportPublicationEnvelope(t.TempDir()))
			if err != nil {
				t.Fatalf("Invoke: %v, want a typed result", err)
			}
			if !errors.Is(adapter.firstErr, ErrInvalidPublication) || errors.Is(adapter.firstErr, ErrInvalidCompletion) {
				t.Fatalf("first validation = %v, want only the publication postcondition", adapter.firstErr)
			}
			if adapter.sideEffectRuns != 1 || len(adapter.repairPrompts) != 1 {
				t.Fatalf("runs=%d repair turns=%d, want one session with exactly one repair turn", adapter.sideEffectRuns, len(adapter.repairPrompts))
			}
			for _, want := range []string{"publish_output", "report", "do not repeat completed external actions"} {
				if !strings.Contains(adapter.repairPrompts[0], want) {
					t.Fatalf("repair prompt missing %q:\n%s", want, adapter.repairPrompts[0])
				}
			}
			if strings.Contains(adapter.repairPrompts[0], "schema validation:") {
				t.Fatalf("repair prompt misdescribes a valid completion as invalid:\n%s", adapter.repairPrompts[0])
			}
			for _, artifact := range rec.artifacts {
				if string(artifact.data) == tc.first || (!tc.wantSuccess && string(artifact.data) == tc.repair) {
					t.Fatalf("rejected payload %q became consumable", artifact.data)
				}
			}
			if tc.wantSuccess {
				if result.Status != apiv1.ResultSuccess || len(result.Artifacts) != 2 {
					t.Fatalf("result=%+v, want the corrected publication accepted", result)
				}
				return
			}
			if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "invalid_declared_artifact_set" ||
				!strings.Contains(result.Error.Message, "schemas/report.schema.json") || len(result.Artifacts) != 0 {
				t.Fatalf("result=%+v, want a terminal producer failure naming the schema", result)
			}
		})
	}
}

func TestExecutorPublicationPostconditionIgnoresNonSuccessCompletion(t *testing.T) {
	adapter := &publicationRepairingAdapter{status: apiv1.ResultFailure, first: `{"summary":3}`}
	result, err := newPostconditionExecutor(t, adapter, &fakeRecorder{}).Invoke(reportSchemaContext(t), reportPublicationEnvelope(t.TempDir()))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if adapter.firstErr != nil || len(adapter.repairPrompts) != 0 {
		t.Fatalf("a failure completion got a publication repair turn: %v", adapter.firstErr)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status=%q", result.Status)
	}
}

func publicationOnceValidator() func([]byte) error {
	calls := 0
	return func([]byte) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("%w: entry %q does not satisfy schema %q: /summary", ErrInvalidPublication, "report", "schemas/report.schema.json")
		}
		return nil
	}
}

func TestClaudeAdapterRepairsInvalidPublicationInSameSession(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	runner := &claudeSequenceRunner{
		results: []ProcessResult{{Transcript: []byte(claudeResultStream)}, {Transcript: []byte(claudeResultStream)}},
		acts:    []func(ProcessRequest) error{writeSuccessCompletion, nil},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}
	out, err := adapter.Run(context.Background(), claudeUncommittedRequest(workspace, publicationOnceValidator()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.reqs) != 2 || !slices.Contains(runner.reqs[1].Command, "--resume") {
		t.Fatalf("process calls = %d, want one resumed repair turn", len(runner.reqs))
	}
	if prompt := string(runner.reqs[1].Stdin); !strings.Contains(prompt, "publish_output") || !strings.Contains(prompt, "/summary") {
		t.Fatalf("repair prompt = %q, want the publication reason and republish instruction", prompt)
	}
	if len(out.InvalidCompletionPayload) != 0 {
		t.Fatalf("a schema-valid completion was captured as invalid: %s", out.InvalidCompletionPayload)
	}
}

func TestCodexAdapterRepairsInvalidPublicationOnOriginalThread(t *testing.T) {
	for _, tc := range []struct {
		name      string
		validate  func([]byte) error
		wantError bool
	}{
		{name: "repaired", validate: publicationOnceValidator()},
		{name: "exhausted keeps completion", validate: func([]byte) error { return ErrInvalidPublication }, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			turns := 0
			runner := &fakeProcessRunner{
				result: ProcessResult{ExitCode: 0, Transcript: []byte(codexCompletedStream)},
				act: func(req ProcessRequest) error {
					turns++
					return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
				},
			}
			adapter := &CodexAdapter{Command: []string{"codex"}, Runner: runner, EnvCapabilities: map[string]string{"agent:model": codexModelEnv}}
			out, err := adapter.Run(context.Background(), RunRequest{
				Envelope: testEnvelope(workspace, "agent:model"), Workspace: workspace, CompletionPath: DefaultResultPath,
				Timeout: time.Minute, Credentials: pushCredentials(t, "agent:model", "sk-test-codex"), ValidateCompletion: tc.validate,
			})
			if turns != 2 || !slices.Contains(runner.lastReq.Command, "resume") || !slices.Contains(runner.lastReq.Command, "thread-1") {
				t.Fatalf("turns=%d last command=%v, want one repair on the original thread", turns, runner.lastReq.Command)
			}
			if !strings.Contains(string(runner.lastReq.Stdin), "publish_output") {
				t.Fatalf("repair prompt = %q", runner.lastReq.Stdin)
			}
			if len(out.InvalidCompletionPayload) != 0 || len(out.Payload) == 0 {
				t.Fatalf("completion handling: invalid=%q payload=%q", out.InvalidCompletionPayload, out.Payload)
			}
			if tc.wantError != (err != nil) || (err != nil && (!errors.Is(err, ErrInvalidPublication) || errors.Is(err, ErrInvalidCompletion))) {
				t.Fatalf("Run error = %v", err)
			}
		})
	}
}

func TestPublicationSettleStripsOnlyThePostcondition(t *testing.T) {
	p := &publicationPostcondition{}
	other := errors.New("read goobers-io receipts")
	if _, err := p.settle(Outcome{}, errors.Join(ErrInvalidPublication, other)); !errors.Is(err, other) || errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("settle = %v", err)
	}
	var unarmed *publicationPostcondition
	if _, err := unarmed.settle(Outcome{}, ErrInvalidPublication); !errors.Is(err, ErrInvalidPublication) {
		t.Fatalf("unarmed settle = %v", err)
	}
}

// A success that is both uncommitted and wrongly published gets one repair
// turn that names both problems, and both postconditions settle cleanly.
func TestPublicationPostconditionCombinesWithCommitPostcondition(t *testing.T) {
	workspace := t.TempDir()
	if err := stageReport(workspace, `{"summary":3}`); err != nil {
		t.Fatal(err)
	}
	e := newPostconditionExecutor(t, &FakeAdapter{}, &fakeRecorder{})
	req := RunRequest{Mode: ModeInvoke, CompletionPath: DefaultResultPath, ValidateCompletion: func([]byte) error {
		return fmt.Errorf("%w; uncommitted paths:\n M main.go", ErrUncommittedChanges)
	}}
	publication := e.armPublicationPostcondition(reportSchemaContext(t), ModeInvoke, reportPublicationEnvelope(workspace), &req)
	err := validateCompletion(req, []byte(`{"status":"success"}`), nil)
	if !errors.Is(err, ErrUncommittedChanges) || !errors.Is(err, ErrInvalidPublication) || errors.Is(err, ErrInvalidCompletion) {
		t.Fatalf("validation = %v, want both postconditions", err)
	}
	prompt := renderCompletionRepairPrompt(req, err)
	for _, want := range []string{"git commit", "publish_output", "main.go", "schemas/report.schema.json"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("repair prompt missing %q:\n%s", want, prompt)
		}
	}
	if _, settled := publication.settle((&commitPostcondition{}).settle(Outcome{}, err)); settled != nil {
		t.Fatalf("settled = %v, want both postconditions cleared", settled)
	}
}
