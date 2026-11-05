package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
)

func tlcFrame(code, severity int, body string) string {
	return fmt.Sprintf("@!@!@STARTMSG %d:%d @!@!@\n%s\n@!@!@ENDMSG %d @!@!@\n", code, severity, body, code)
}

func tlcSuccessfulOutput() string {
	return tlcFrame(2187, 0, "breadth-first model checking") + tlcFrame(2185, 0, "Starting...") +
		tlcFrame(2190, 0, "Finished computing initial states: 1 distinct state generated") + tlcFrame(2193, 0, "Model checking completed.") +
		tlcFrame(2199, 0, "3 states generated, 2 distinct states found, 0 states left on queue.") + tlcFrame(2186, 0, "Finished")
}

func TestTLCFailClosedClassification(t *testing.T) {
	success := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Metrics: map[string]float64{"exitCode": 0}}
	violation := tlcFrame(2187, 0, "breadth-first") + tlcFrame(2185, 0, "Starting...") +
		tlcFrame(2110, 1, "Invariant Safe is violated.") + tlcFrame(2121, 1, "The behavior up to this point is:") +
		tlcFrame(2217, 4, "state = 2") + tlcFrame(2186, 0, "Finished")
	nonzero := apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: "nonzero_exit"}, Metrics: map[string]float64{"exitCode": 12}}
	for _, test := range []struct {
		name, stdout, stderr string
		result               apiv1.ResultEnvelope
		want                 creditgraph.VerificationOutcome
	}{
		{"proven", tlcSuccessfulOutput(), "", success, creditgraph.VerificationProven},
		{"counterexample", violation, "", nonzero, creditgraph.VerificationCounterexample},
		{"exit-zero-alone", "", "", success, creditgraph.VerificationInconclusive},
		{"empty-initial-obligations", strings.ReplaceAll(tlcSuccessfulOutput(), "1 distinct state generated", "0 distinct states generated"), "", success, creditgraph.VerificationInconclusive},
		{"truncated-frame", tlcSuccessfulOutput() + "@!@!@STARTMSG 1:0 @!@!@\n", "", success, creditgraph.VerificationInconclusive},
		{"simulation", tlcSuccessfulOutput() + tlcFrame(2188, 0, "simulation"), "", success, creditgraph.VerificationInconclusive},
		{"assumption", tlcSuccessfulOutput() + tlcFrame(2104, 1, "Assumption is false"), "", success, creditgraph.VerificationInconclusive},
		{"incomplete", strings.ReplaceAll(tlcSuccessfulOutput(), "0 states left", "1 states left"), "", success, creditgraph.VerificationInconclusive},
		{"crash", tlcSuccessfulOutput(), "Exception in thread", success, creditgraph.VerificationInconclusive},
		{"checker-bug", tlcSuccessfulOutput() + tlcFrame(1000, 2, "TLC bug"), "", success, creditgraph.VerificationInconclusive},
		{"wrong-property", strings.ReplaceAll(violation, "Safe", "Other"), "", nonzero, creditgraph.VerificationInconclusive},
		{"no-trace", strings.ReplaceAll(violation, tlcFrame(2217, 4, "state = 2"), ""), "", nonzero, creditgraph.VerificationInconclusive},
		{"timeout-after-success", tlcSuccessfulOutput(), "", apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: "timeout"}}, creditgraph.VerificationInconclusive},
		{"output-limit", tlcSuccessfulOutput(), "", apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{StdoutTruncatedOutput: true}}, creditgraph.VerificationInconclusive},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, reason := classifyTLC([]byte(test.stdout), []byte(test.stderr), test.result, "Safe")
			if got != test.want || got == creditgraph.VerificationInconclusive && reason == "" {
				t.Fatalf("outcome = %s (%s), want %s", got, reason, test.want)
			}
		})
	}
}

func TestTLCModelRejectsExecutableAndIncompleteInputs(t *testing.T) {
	for _, input := range []string{
		`{"states":[0],"initial":[],"edges":[]}`,
		`{"states":[0,0],"initial":[0],"edges":[]}`,
		`{"states":[0],"initial":[1],"edges":[]}`,
		`{"states":[0],"initial":[0],"edges":[[0,1]]}`,
		`{"states":[0],"initial":[0],"edges":[[0]]}`,
		`{"states":[0],"initial":[0],"edges":[[0,0,0]]}`,
		`{"states":[0],"initial":[0],"edges":[],"assumptions":["FALSE"]}`,
		`{"states":["PrintT(\"success\")"],"initial":[0],"edges":[]}`,
		`{"states":[0],"initial":[0],"edges":[]} {}`,
	} {
		if _, err := compileTLCModel([]byte(input)); err == nil {
			t.Fatalf("accepted unsafe or incomplete graph: %s", input)
		}
	}
	module, err := compileTLCModel([]byte(`{"states":[0],"initial":[0],"edges":[]}`))
	if err != nil || !strings.Contains(string(module), "Next ==\n  FALSE") {
		t.Fatalf("valid finite dead end = %s, %v", module, err)
	}
}

func tlcTestDirectory(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp(".", ".tlc-test-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func TestTLCPinnedInputAndRuntimeSnapshots(t *testing.T) {
	root := tlcTestDirectory(t)
	source := filepath.Join(root, "runtime")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "java")
	original := []byte("reviewed executable")
	if err := os.WriteFile(path, original, 0o700); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(root, "snapshot")
	files, err := snapshotTLCRuntime(source, copied)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := readTLCPinned(filepath.Join(copied, "java"), tlcDigest(original), 1024)
	if err != nil || string(data) != string(original) || files[0].Digest != tlcDigest(original) {
		t.Fatalf("executed snapshot did not pin copied bytes: %s, %v", data, err)
	}
	if _, err := readTLCPinned(path, tlcDigest(original), 1024); err == nil {
		t.Fatal("changed dependency digest accepted")
	}
	if _, err := readTLCFile(path, 1); err == nil {
		t.Fatal("oversized input accepted")
	}
}

type tlcTestJournal struct {
	*fakeRecorder
	fail bool
}

func (j *tlcTestJournal) Append(event journal.Event) error {
	if j.fail {
		return errors.New("journal unavailable")
	}
	event.Seq = uint64(len(j.events) + 1)
	return j.fakeRecorder.Append(event)
}

func (j *tlcTestJournal) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	ref, err := j.fakeRecorder.RecordArtifact(name, data)
	if err != nil {
		return ref, err
	}
	return ref, j.Append(journal.Event{Type: journal.EventArtifactRecorded, Name: name, Ref: &ref})
}

func TestTLCPublicationRecordsBoundArtifactsBeforeAnnotation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		recorder := &tlcTestJournal{fakeRecorder: newFakeRecorder()}
		env := apiv1.InvocationEnvelope{TaskID: "check", Attempt: 1}
		if err := recorder.Append(journal.Event{Type: journal.EventStageStarted, Stage: "check", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
		source, err := recorder.RecordArtifact("check/stdout.log", []byte(tlcSuccessfulOutput()))
		if err != nil {
			t.Fatal(err)
		}
		a := &tlcAttempt{subject: []byte("graph"), spec: []byte("spec"), config: []byte("config"), module: []byte("compiled"),
			manifest: TLCManifest{Property: "safe", Assumptions: []string{}}}
		executor := &TLCExecutor{Shell: &ShellExecutor{Journal: recorder}}
		recorder.fail = fail
		result, err := executor.publishTLC(env, a, &tlcCapture{source: source}, map[string]any{"result": "success"}, creditgraph.VerificationProven, "")
		if fail {
			if err == nil {
				t.Fatal("journal failure was hidden")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := recorder.Append(journal.Event{Type: journal.EventStageFinished, Stage: "check", Attempt: 1, Status: string(result.Status)}); err != nil {
			t.Fatal(err)
		}
		graph, err := creditgraph.Build(creditgraph.Input{RunID: "check", Events: recorder.events})
		if err != nil || len(graph.Verifications) != 1 || len(graph.UnverifiedVerifications) != 0 {
			t.Fatalf("bound checker evidence not accepted: %+v, %v", graph, err)
		}
		var context map[string]any
		if err := json.Unmarshal(recorder.recorded["check/context.json"], &context); err != nil || context["adapter"] != "tlc-finite-graph-v1" {
			t.Fatalf("missing adapter provenance: %v", err)
		}
	}

}

func TestTLCReviewedInputsAndCandidateStaySeparate(t *testing.T) {
	root := tlcTestDirectory(t)
	reviewed, candidate := filepath.Join(root, "reviewed"), filepath.Join(root, "candidate")
	for _, directory := range []string{reviewed, candidate} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	spec := []byte("---- MODULE Spec ----\nEXTENDS Candidate\nSafe == state # 2\n====\n")
	cfg, err := tlcConfiguration("Safe")
	if err != nil {
		t.Fatal(err)
	}
	model := []byte(`{"states":[0],"initial":[0],"edges":[[0,0]]}`)
	for path, data := range map[string][]byte{
		filepath.Join(reviewed, "Spec.tla"): spec, filepath.Join(reviewed, "Spec.cfg"): cfg, filepath.Join(candidate, "model.json"): model,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := TLCManifest{Property: "safe", Invariant: "Safe", Specification: "Spec.tla", Configuration: "Spec.cfg",
		SpecDigest: tlcDigest(spec), ConfigDigest: tlcDigest(cfg), Assumptions: []string{}}
	env := apiv1.InvocationEnvelope{Workspace: candidate, Inputs: map[string]any{InputTLCModel: "model.json"}}
	snapshot := &tlcAttempt{manifest: manifest}
	if err := snapshot.readInputs(reviewed, env); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reviewed, "Spec.tla"), []byte("weakened"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "model.json"), []byte(`{"states":[2],"initial":[2],"edges":[[2,2]]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if string(snapshot.spec) != string(spec) || string(snapshot.subject) != string(model) {
		t.Fatal("snapshot reread changed live inputs")
	}
	fresh := &tlcAttempt{manifest: manifest}
	if err := fresh.readInputs(reviewed, env); err == nil {
		t.Fatal("changed reviewed property accepted")
	}
	for _, text := range []string{
		string(spec) + "\nASSUME FALSE\n",
		string(spec) + "\nTHEOREM Unsafe == TRUE\nPROOF OMITTED\n",
		string(spec) + "\nINSTANCE Other WITH Safe <- TRUE\n",
		strings.ReplaceAll(string(spec), "EXTENDS Candidate", "EXTENDS Candidate, IOUtils"),
		string(spec) + "\n---- MODULE IOUtils ----\n====\n",
	} {
		if err := os.WriteFile(filepath.Join(reviewed, "Spec.tla"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		fresh.manifest.SpecDigest = tlcDigest([]byte(text))
		if err := fresh.readInputs(reviewed, env); err == nil {
			t.Fatal("unsupported assumptions or proof obligations accepted")
		}
	}
	if err := os.WriteFile(filepath.Join(reviewed, "Spec.tla"), spec, 0o600); err != nil {
		t.Fatal(err)
	}
	fresh.manifest = manifest
	weakened := append(append([]byte(nil), cfg...), []byte("CONSTRAINT FALSE\n")...)
	if err := os.WriteFile(filepath.Join(reviewed, "Spec.cfg"), weakened, 0o600); err != nil {
		t.Fatal(err)
	}
	fresh.manifest.ConfigDigest = tlcDigest(weakened)
	if err := fresh.readInputs(reviewed, env); err == nil {
		t.Fatal("pinned but incomplete obligation selection accepted")
	}
}

func TestTLCRefusesUntrustedInvocationControls(t *testing.T) {
	base := apiv1.InvocationEnvelope{Inputs: map[string]any{InputTLCManifest: "manifest.json", InputTLCModel: "model.json"}}
	marker := apiv1.DeterministicRun{Command: []string{KindTLC}}
	if err := validateTLCInvocation(base, marker); err != nil {
		t.Fatal(err)
	}
	for _, run := range []apiv1.DeterministicRun{
		{},
		{Command: []string{"fake-checker"}},
		{Command: []string{"TLC"}},
		{Command: []string{"tlc", "-simulate"}},
		{Command: []string{"tlc", ""}},
		{Command: []string{KindTLC}, Script: "echo proven"},
		{Command: []string{KindTLC}, Env: map[string]string{"JAVA_TOOL_OPTIONS": "-javaagent:untrusted.jar"}},
		{Command: []string{KindTLC}, InjectRunContext: true},
	} {
		if err := validateTLCInvocation(base, run); err == nil {
			t.Fatalf("agent control was accepted: %+v", run)
		}
	}
	base.Capabilities = []string{"repo:push"}
	if err := validateTLCInvocation(base, marker); err == nil {
		t.Fatal("credential capability was accepted")
	}
	base.Capabilities = nil
	base.Inputs[InputVerificationReport] = true
	if err := validateTLCInvocation(base, marker); err == nil {
		t.Fatal("agent-authored report path was accepted")
	}
	_, err := (&TLCExecutor{Shell: &ShellExecutor{Journal: newFakeRecorder(), ConfigDirectory: "missing"}}).Run(
		t.Context(), apiv1.InvocationEnvelope{Workspace: "candidate", Inputs: map[string]any{InputTLCManifest: "manifest.json", InputTLCModel: "model.json"}}, marker)
	if err == nil {
		t.Fatal("missing trusted inputs produced a report")
	}
}
