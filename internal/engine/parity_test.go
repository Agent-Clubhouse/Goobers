package engine

// The runner-vs-engine PARITY HARNESS (finding 002 item P0, decision 005).
//
// internal/engine/conformance_test.go already diffs the two runners' JOURNALS
// over journal.ConformanceView for hand-written fixtures. That surface is
// necessary but not sufficient for the mode-3 pivot: the parity inventory's
// rows are mostly about what the runner puts INTO a stage — the invocation
// envelope — and about the terminal Result fields the scheduler reads back.
// A row like "backlog-query requireLabels defaulting" is invisible to a
// journal diff (both journals say stage.finished success) and lethal in
// production (the cloud instance claims items the local partition owns).
//
// So this harness compares THREE surfaces for one shared fixture:
//
//  1. ENVELOPES: every apiv1.InvocationEnvelope handed to an executor, in
//     dispatch order, normalized (parityEnvelope) to drop the fields that
//     legitimately differ between a local worktree and a worker workspace.
//  2. JOURNALS: journal.ConformanceView, reusing diffConformanceViews from
//     the conformance harness rather than a second formatter.
//  3. TERMINAL: the fields the daemon's Starter maps into StartResult —
//     status, final state, failure code, and NoWork.
//
// EXPECTED FAILURES. A parity row that is KNOWN-BROKEN on the engine today is
// not skipped: t.Skip makes a red row indistinguishable from an absent one and
// nothing forces its removal when the port lands. Instead every case names its
// finding-002 inventory row id, and parityExpectedFailures is a documented list
// the harness asserts AGAINST in both directions:
//
//   - a row that fails and is NOT on the list fails the test (a regression);
//   - a row that PASSES and IS on the list fails the test (the port landed —
//     delete the entry).
//
// That second direction is the whole point: a port that fixes a row must also
// delete it from this list or CI goes red.
//
// SEAM FOR LATER PORTS. Each row lives in its own parity_row_<row>_test.go
// file and registers itself with registerParityRow in an init function. To add
// a failing-first case for a port:
//
//  1. add a parityRow constant naming the finding-002 inventory row and the
//     plan item that closes it (e.g. "E4-cached-verdict");
//  2. create parity_row_<slug>_test.go with an init that calls
//     registerParityRow, a Build (for a lane-derived fixture) or a literal
//     Spec, and a Check that asserts the RUNNER side really exhibits the
//     behaviour before diffing — otherwise the row can pass with both sides
//     equally wrong;
//  3. add one line to parityExpectedFailures with the reason and the plan
//     item, and confirm the row logs "expected failure, still open";
//  4. when the port lands, DELETE that line. CI fails if you do not.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// --- inventory rows ---------------------------------------------------------

// parityRow is a finding-002 parity-inventory row id. It is the join key
// between this harness, parityExpectedFailures, and the port that closes the
// row — deliberately a named string type so a typo is a compile error at the
// registration site rather than a silently orphaned expectation.
type parityRow string

const (
	// rowLaneBacklogCuration is not an inventory gap: it is the P0 baseline
	// that proves the harness walks a real production lane end to end on both
	// runners. It must stay GREEN; a port that breaks it broke the lane.
	rowLaneBacklogCuration parityRow = "P0-lane-backlog-curation"
	// rowLaneImplementation is the second lane baseline (finding 002 R11's
	// second cutover) and the DECLARED-WINS half of the backlog-query
	// defaulting contract. It must stay GREEN.
	rowLaneImplementation parityRow = "P0-lane-implementation"
	// rowInputsFromBareKey is the P0 far-side golden named in the plan: bare
	// (including legacy dotted) inputsFrom keys resolve identically on both
	// runners. It must stay GREEN.
	rowInputsFromBareKey parityRow = "P0-inputsfrom-bare-key"
	// rowBacklogQueryDefaults is inventory row "backlog-query input
	// defaulting" (plan item E1): gaggle RequireLabels + self-identity
	// assignedTo injected into every goobers backlog-query stage.
	rowBacklogQueryDefaults parityRow = "E1-backlog-query-defaults"
	// rowRunResultNoWork is inventory row "NoWork short-circuit accounting"
	// (plan item E2): Result.NoWork is true only for a terminal no-work at
	// step 1, and the scheduler's idle backoff reads it.
	rowRunResultNoWork parityRow = "E2-runresult-nowork"
	// rowRunResultNoWorkLateStage is the negative half of the same inventory
	// row: a no-work reached after step 1 must NOT set the accounting. It
	// already agrees on both sides and must stay GREEN, so the port that
	// closes the positive half cannot over-set the flag unnoticed.
	rowRunResultNoWorkLateStage parityRow = "E2-runresult-nowork-late-stage"
	// rowInputsFromStageQualified is inventory row "Stage-qualified inputsFrom
	// (#562)" (plan item E2): "<stage>.<key>" resolves against ANY completed
	// stage's outputs on a DSL version that supports it.
	rowInputsFromStageQualified parityRow = "E2-inputsfrom-stage-qualified"
)

// parityExpectedFailures is the DOCUMENTED expected-failure list: parity rows
// that are red on the engine path today, each with the reason and the plan
// item that closes it. The harness asserts this list in both directions (see
// TestParityRunnerVsEngine), so:
//
//   - deleting a row's port without deleting its entry here fails CI;
//   - landing a port without deleting its entry here ALSO fails CI.
//
// Do not add an entry to silence a regression. An entry is only legitimate for
// a row the parity inventory names as a known, planned gap.
var parityExpectedFailures = map[parityRow]string{
	rowBacklogQueryDefaults: "engine RunInput has no BacklogQueryAssignedTo/RequireLabels; " +
		"internal/runner/run.go:4413-4414 applies them in dispatchTask and the engine's runTask has no counterpart. Closed by plan item E1.",
	rowRunResultNoWork: "engine.RunResult has no NoWork field (engine.go:131-141); the local runner sets " +
		"Result.NoWork = steps == 1 (run.go:3606) and localscheduler's idle backoff reads it. Closed by plan item E2.",
	rowInputsFromStageQualified: "engine runTask resolves inputsFrom only against the immediately preceding task's " +
		"Outputs (engine.go:555-561); the local runner resolves \"<stage>.<key>\" against any completed stage " +
		"(internal/runner/inputsfrom.go:78-89) when workflow.SupportsStageQualifiedInputs holds. Closed by plan item E2.",
}

// --- case registration ------------------------------------------------------

// parityCase is one row of the parity table: a shared workflow fixture, the
// scripted stage behaviour behind BOTH runners, the runner-side configuration
// the engine may or may not have a counterpart for, and the row's own
// assertion over the two observations.
type parityCase struct {
	// Row is the finding-002 inventory row this case pins.
	Row parityRow
	// Name is the subtest name.
	Name string
	// Lane names the production lane the fixture derives from, for the
	// failure message ("" for a synthetic fixture).
	Lane string
	// Build populates the case just before it runs. Rows derived from a
	// production lane need it because loading the lane needs *testing.T; a
	// purely synthetic row can fill the fields at registration instead.
	Build func(t *testing.T, c *parityCase)
	// Spec is the workflow definition walked by both runners.
	Spec apiv1.WorkflowSpec
	// DSLVersion is the definition's declared language version. It is
	// load-bearing: workflow.SupportsStageQualifiedInputs keys on it.
	DSLVersion string
	// Script is the per-stage scripted behaviour (scriptedExec, shared with
	// the conformance harness).
	Script map[string][]scriptedCall
	// MaxRepasses is the run's repass budget on both sides.
	MaxRepasses int
	// BacklogQueryAssignedTo / BacklogQueryRequireLabels are the local
	// runner's gaggle-identity defaults. The engine has no counterpart yet —
	// that absence IS rowBacklogQueryDefaults.
	BacklogQueryAssignedTo    string
	BacklogQueryRequireLabels string
	// UsesRepo marks a fixture whose stages take a repo workspace, so the
	// local side needs the hermetic git fixture repo.
	UsesRepo bool
	// Check is the row's own assertion. It returns an error rather than
	// calling t.Fatal so the expected-failure list can grade it. Nil means
	// "the three default surfaces must match" (checkAllSurfaces).
	Check func(obs parityObservation) error
}

var (
	parityRegistryMu sync.Mutex
	parityRegistry   []parityCase
)

// registerParityRow adds one case to the table. Row files call it from init so
// adding a failing-first case for a port is a single new file.
func registerParityRow(c parityCase) {
	parityRegistryMu.Lock()
	defer parityRegistryMu.Unlock()
	parityRegistry = append(parityRegistry, c)
}

// parityCases returns the registered table in row-id order, so the suite runs
// deterministically regardless of init file ordering.
func parityCases() []parityCase {
	parityRegistryMu.Lock()
	defer parityRegistryMu.Unlock()
	out := append([]parityCase(nil), parityRegistry...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Row != out[j].Row {
			return out[i].Row < out[j].Row
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// --- observation ------------------------------------------------------------

// parityEnvelope is the cross-runner comparable projection of an
// InvocationEnvelope, mirroring journal.NormativeEvent's design: flat, and
// excluding exactly the fields that CANNOT agree between a local worktree walk
// and a worker/pod walk. Everything else is normative — a difference is a
// parity bug, not harness noise.
//
// Excluded, with the reason:
//   - Workspace: an absolute path minted per attempt by each side's
//     provisioner (runner: worktree.Manager; engine: the activity's
//     WorkspaceProvisioner). Never the same string, never can be.
//   - Attempt: infra retries renumber attempts independently on each side;
//     journal.ConformanceView excludes infra-retry attempts for the same
//     reason. Dispatch ORDER is preserved by the slice itself.
//   - ContextPointers' Artifact.Path/Digest/Size: journal-relative locations
//     and content digests of bytes each side wrote itself. The pointer NAME
//     and Integrity grade are what a stage's admission and evidence depend
//     on, and both are compared (ContextPointers below).
type parityEnvelope struct {
	Stage             string
	WorkflowID        string
	Goal              string
	Goober            string
	Gaggle            string
	BranchNamespace   string
	BaseBranch        string
	TriggerRef        string
	OwnershipBoundary string
	MinimumIntegrity  apiv1.Integrity
	// Inputs is a stable encoding of the stage's resolved inputs (declared
	// Inputs, then inputsFrom overlays, then runner-side defaulting). This is
	// the field most parity rows turn on.
	Inputs string
	// Capabilities/PolicyActions are declaration-ordered, joined.
	Capabilities  string
	PolicyActions string
	// ContextPointers encodes name+integrity+kind per pointer, in order.
	ContextPointers string
	// Item encodes the backlog item's normative identity, empty when absent.
	Item string
}

// String prints EVERY compared field. A projection that compares a field it
// does not print produces the worst possible failure message — "these two
// identical-looking envelopes differ" — so the two must stay in lockstep;
// TestParityEnvelopeStringPrintsEveryComparedField enforces that.
func (e parityEnvelope) String() string {
	return fmt.Sprintf("stage=%s workflowId=%s gaggle=%s goal=%q goober=%s ownership=%s "+
		"branchNamespace=%q baseBranch=%q triggerRef=%q minIntegrity=%q inputs=[%s] caps=[%s] policy=[%s] pointers=[%s] item=%q",
		e.Stage, e.WorkflowID, e.Gaggle, e.Goal, e.Goober, e.OwnershipBoundary,
		e.BranchNamespace, e.BaseBranch, e.TriggerRef, e.MinimumIntegrity,
		e.Inputs, e.Capabilities, e.PolicyActions, e.ContextPointers, e.Item)
}

// stageOf extracts the stage name from an envelope TaskID ("<runID>:<stage>"),
// the same split the scripted executors use.
func stageOf(taskID string) string {
	return taskID[strings.Index(taskID, ":")+1:]
}

func projectParityEnvelope(env apiv1.InvocationEnvelope) parityEnvelope {
	return parityEnvelope{
		Stage:             stageOf(env.TaskID),
		WorkflowID:        env.WorkflowID,
		Goal:              env.Goal,
		Goober:            env.Goober,
		Gaggle:            env.Gaggle,
		BranchNamespace:   env.BranchNamespace,
		BaseBranch:        env.BaseBranch,
		TriggerRef:        env.TriggerRef,
		OwnershipBoundary: env.OwnershipBoundary,
		MinimumIntegrity:  env.MinimumIntegrity,
		Inputs:            encodeParityInputs(env.Inputs),
		Capabilities:      strings.Join(env.Capabilities, ","),
		PolicyActions:     strings.Join(env.PolicyActions, ","),
		ContextPointers:   encodeParityPointers(env.ContextPointers),
		Item:              encodeParityItem(env.Item),
	}
}

// encodeParityInputs renders resolved inputs as a stable, key-sorted string.
// Values go through JSON so a string "1" and a number 1 do not compare equal —
// inputsFrom binds real output values, and their TYPE is part of the contract
// the stage sees.
func encodeParityInputs(inputs map[string]interface{}) string {
	if len(inputs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		encoded, err := json.Marshal(inputs[k])
		if err != nil {
			encoded = []byte(fmt.Sprintf("%q", fmt.Sprint(inputs[k])))
		}
		parts = append(parts, k+"="+string(encoded))
	}
	return strings.Join(parts, " ")
}

func encodeParityPointers(pointers []apiv1.ContextPointer) string {
	if len(pointers) == 0 {
		return ""
	}
	parts := make([]string, 0, len(pointers))
	for _, p := range pointers {
		kind := "none"
		switch {
		case p.Artifact != nil:
			kind = "artifact"
		case p.External != nil:
			kind = "external"
		}
		parts = append(parts, fmt.Sprintf("%s:%s:%s", p.Name, kind, p.Integrity))
	}
	return strings.Join(parts, " ")
}

func encodeParityItem(item *apiv1.BacklogItem) string {
	if item == nil {
		return ""
	}
	return fmt.Sprintf("%s:%s:%s", item.ID, item.Title, item.Integrity)
}

// parityTerminal is the terminal outcome as the daemon's Starter reads it back
// — the StartResult mapping finding 002's D1 describes.
type parityTerminal struct {
	Status      string
	FinalState  string
	FailureCode string
	// NoWork is the #233 short-circuit accounting the scheduler's idle
	// backoff consumes.
	NoWork bool
}

func (t parityTerminal) String() string {
	return fmt.Sprintf("status=%s finalState=%s failureCode=%q noWork=%t", t.Status, t.FinalState, t.FailureCode, t.NoWork)
}

// paritySide is one runner's observation of the shared fixture.
type paritySide struct {
	// Name is "runner" or "engine", used in failure messages.
	Name string
	// Envelopes are the projected invocation envelopes in dispatch order.
	Envelopes []parityEnvelope
	// Events is the side's run journal.
	Events []journal.Event
	// Terminal is the run's terminal outcome.
	Terminal parityTerminal
	// Err is the walk error, if any.
	Err error
}

// parityObservation is both sides of one case.
type parityObservation struct {
	Case   parityCase
	Runner paritySide
	Engine paritySide
}

// --- recording executor -----------------------------------------------------

// recordingExec is scriptedExec plus envelope capture. It sits behind the
// deterministic AND agentic invoke seams on both runners, which is what makes
// the envelope comparison a genuine same-fixture diff rather than two
// separately-constructed expectations.
type recordingExec struct {
	inner *scriptedExec
	mu    sync.Mutex
	envs  []apiv1.InvocationEnvelope
}

func newRecordingExec(script map[string][]scriptedCall) *recordingExec {
	return &recordingExec{inner: newScriptedExec(script)}
}

func (r *recordingExec) record(env apiv1.InvocationEnvelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.envs = append(r.envs, env)
}

func (r *recordingExec) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	r.record(env)
	return r.inner.Run(ctx, env, run)
}

func (r *recordingExec) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	r.record(env)
	return r.inner.Invoke(ctx, env)
}

func (r *recordingExec) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return r.inner.Review(ctx, env)
}

// boundTo returns the agentic seam for one named goober.
//
// The two runners carry goober identity by DIFFERENT, both-correct routes, and
// this is the seam that makes them comparable rather than the harness pretending
// the difference away. The engine puts the name on the wire
// (InvocationEnvelope.Goober — a Temporal worker has only the envelope and
// cannot otherwise know which goober to construct, per the field's own doc).
// The local runner passes it out of band, as the first argument to
// Config.NewAgentic, and leaves env.Goober empty.
//
// Dropping Goober from the comparison would therefore lose the ability to
// assert that the engine dispatches to the SAME goober the runner does — which
// is exactly the identity the critic's WF-016 goober-pin row is about. So the
// runner side stamps the factory's argument onto the envelope it records: the
// compared field then means "which goober did this side dispatch to", which is
// true of both wire shapes.
func (r *recordingExec) boundTo(goober string) invoke.Goober {
	return &boundRecordingExec{owner: r, goober: goober}
}

type boundRecordingExec struct {
	owner  *recordingExec
	goober string
}

func (b *boundRecordingExec) stamp(env apiv1.InvocationEnvelope) apiv1.InvocationEnvelope {
	if env.Goober == "" {
		env.Goober = b.goober
	}
	return env
}

func (b *boundRecordingExec) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	env = b.stamp(env)
	b.owner.record(env)
	return b.owner.inner.Invoke(ctx, env)
}

func (b *boundRecordingExec) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return b.owner.inner.Review(ctx, b.stamp(env))
}

func (r *recordingExec) projected() []parityEnvelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]parityEnvelope, 0, len(r.envs))
	for _, env := range r.envs {
		out = append(out, projectParityEnvelope(env))
	}
	return out
}

// --- execution --------------------------------------------------------------

func parityDefinition(c parityCase) wf.Definition {
	return wf.Definition{Name: parityWorkflowName, DSLVersion: c.DSLVersion, Version: 1, Spec: c.Spec}
}

// parityWorkflowName is the definition name both sides register the fixture
// under, so envelope WorkflowID compares equal.
const parityWorkflowName = "parity"

// parityRepoRef is the shared target repo. Branch is set explicitly because
// the engine's buildInvocation defaults an empty branch to "main" while the
// runner reads it from the RepoRef — pinning it keeps BaseBranch a real
// comparison instead of an accidental agreement.
var parityRepoRef = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}

// parityBranchNamespace is the gaggle's run-branch namespace, NORMALIZED
// exactly as cmd/goobers' branchNamespacesByGaggle normalizes it before either
// starter pins it (runnerwiring.go:784).
//
// It is pinned on both sides on purpose. The local runner normalizes an unset
// namespace itself (run.go:1357 -> providers.NormalizeBranchNamespace), while
// the engine takes RunInput.BranchNamespace verbatim into the envelope
// (engine.go's buildInvocation), so a harness that left it empty would compare
// "goobers/" against "" and report a divergence that production does not have
// — the normalization is the STARTER's job and both starters do it. Pinning
// the same normalized value here keeps the envelope surface about behaviour
// rather than about which side happens to default.
var parityBranchNamespace = providers.NormalizeBranchNamespace("")

// runParityRunnerSide walks the fixture through the real local runner.
func runParityRunnerSide(t *testing.T, c parityCase, runID string) paritySide {
	t.Helper()
	exec := newRecordingExec(c.Script)
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	cfg := runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return exec, nil
		},
		NewAgentic: func(goober string, _ runner.ArtifactRecorder, _ runner.SecretRegistrar) (invoke.Goober, error) {
			return exec.boundTo(goober), nil
		},
		Automated:                 gate.NewAutomatedEvaluator(),
		MaxRepasses:               c.MaxRepasses,
		Worktrees:                 wtMgr,
		RunsDir:                   runsDir,
		ScratchDir:                filepath.Join(instanceRoot, "scratch"),
		BacklogQueryAssignedTo:    c.BacklogQueryAssignedTo,
		BacklogQueryRequireLabels: c.BacklogQueryRequireLabels,
		BranchNamespaces:          map[string]string{c.Spec.Gaggle: parityBranchNamespace},
	}
	if c.UsesRepo {
		repo := newConformanceFixtureRepo(t)
		cfg.RepoCloneURL = func(apiv1.RepoRef) (string, error) { return repo, nil }
	}
	r, err := runner.New(cfg)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	machine, err := wf.Compile(parityDefinition(c), wf.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile fixture for row %s: %v", c.Row, err)
	}
	res, startErr := r.Start(context.Background(), runner.StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  c.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: parityRepoRef,
	})
	side := paritySide{
		Name:      "runner",
		Envelopes: exec.projected(),
		Events:    readJournalEvents(t, filepath.Join(runsDir, runID)),
		Err:       startErr,
	}
	// A walk that errored has no terminal to compare; diffParityWalkOutcome
	// reports the asymmetry instead, and the zero terminal keeps the
	// comparison from inventing a phase the runner never reached.
	if startErr == nil {
		side.Terminal = parityTerminal{
			Status:      statusForPhase(t, res.Phase),
			FinalState:  res.FinalState,
			FailureCode: res.FailureCode,
			NoWork:      res.NoWork,
		}
	}
	return side
}

// runParityEngineSide walks the same fixture through the engine in Temporal's
// test environment and projects its history into a journal (#629), exactly as
// the conformance harness does.
func runParityEngineSide(t *testing.T, c parityCase, runID string) paritySide {
	t.Helper()
	exec := newRecordingExec(c.Script)
	in := RunInput{
		RunID:                  runID,
		Gaggle:                 c.Spec.Gaggle,
		WorkflowName:           parityWorkflowName,
		Version:                1,
		DSLVersion:             c.DSLVersion,
		PreviewFeaturesEnabled: boolPointer(true),
		Spec:                   c.Spec,
		RepoRef:                parityRepoRef,
		TriggerKind:            string(journal.TriggerManual),
		MaxRepasses:            c.MaxRepasses,
		BranchNamespace:        parityBranchNamespace,
	}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.SetStartTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	env.RegisterActivity(&Activities{
		Goober:     exec,
		Det:        exec,
		Auto:       gate.NewAutomatedEvaluator(),
		Workspaces: testWorkspaces(t),
	})
	env.ExecuteWorkflow(Run, in)
	workflowErr := env.GetWorkflowError()
	side := paritySide{
		Name:      "engine",
		Envelopes: exec.projected(),
		Events:    projectEngineJournal(t, env),
		Err:       workflowErr,
	}
	if workflowErr == nil {
		var res RunResult
		if err := env.GetWorkflowResult(&res); err != nil {
			t.Fatalf("engine result: %v", err)
		}
		side.Terminal = parityTerminal{
			Status:      res.Status,
			FinalState:  res.FinalState,
			FailureCode: res.FailureCode,
			NoWork:      engineRunResultNoWork(t, res),
		}
	}
	return side
}

// engineRunResultNoWork reads RunResult's NoWork accounting WITHOUT compiling
// against a field that does not exist yet.
//
// This is the seam that makes rowRunResultNoWork a genuine failing-first case:
// the harness cannot reference res.NoWork today (it would not compile), and
// hard-coding false would make the row pass by construction the moment someone
// "fixed" the expectation instead of the engine. Decoding the marshalled
// result and looking for the field the plan specifies ("noWork", omitempty)
// means the row flips to green the instant E2 adds
//
//	NoWork bool `json:"noWork,omitempty"`
//
// to RunResult and sets it — with no edit to this harness.
func engineRunResultNoWork(t *testing.T, res RunResult) bool {
	t.Helper()
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal engine RunResult: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode engine RunResult: %v", err)
	}
	raw, ok := fields["noWork"]
	if !ok {
		return false
	}
	var noWork bool
	if err := json.Unmarshal(raw, &noWork); err != nil {
		t.Fatalf("engine RunResult noWork is not a bool (%s): %v", raw, err)
	}
	return noWork
}

// projectEngineJournal turns the test environment's accumulated projection
// into journal events, the same path runEngineFixture uses.
func projectEngineJournal(t *testing.T, env *testsuite.TestWorkflowEnvironment) []journal.Event {
	t.Helper()
	val, err := env.QueryWorkflow(JournalQuery)
	if err != nil {
		t.Fatalf("query journal projection: %v", err)
	}
	var proj JournalProjection
	if err := val.Get(&proj); err != nil {
		t.Fatalf("decode journal projection: %v", err)
	}
	dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	return readJournalEvents(t, dir)
}

// --- surfaces ---------------------------------------------------------------

// diffParityEnvelopes compares the two dispatch sequences position by
// position, naming the first divergence and printing both envelopes.
func diffParityEnvelopes(obs parityObservation) error {
	runnerEnvs, engineEnvs := obs.Runner.Envelopes, obs.Engine.Envelopes
	limit := len(runnerEnvs)
	if len(engineEnvs) < limit {
		limit = len(engineEnvs)
	}
	for i := 0; i < limit; i++ {
		if runnerEnvs[i] != engineEnvs[i] {
			return fmt.Errorf("stage envelopes diverge at dispatch %d:\n  runner: %s\n  engine: %s",
				i+1, runnerEnvs[i], engineEnvs[i])
		}
	}
	if len(runnerEnvs) != len(engineEnvs) {
		longerName, longer := "engine", engineEnvs
		if len(runnerEnvs) > len(engineEnvs) {
			longerName, longer = "runner", runnerEnvs
		}
		return fmt.Errorf("stage envelopes diverge at dispatch %d: %s dispatched %d more stage(s), first extra:\n  %s",
			limit+1, longerName, len(longer)-limit, longer[limit])
	}
	return nil
}

// diffParityTerminal compares the terminal outcome the daemon's Starter maps
// into StartResult.
func diffParityTerminal(obs parityObservation) error {
	if obs.Runner.Terminal != obs.Engine.Terminal {
		return fmt.Errorf("terminal outcomes diverge:\n  runner: %s\n  engine: %s",
			obs.Runner.Terminal, obs.Engine.Terminal)
	}
	return nil
}

// diffParityWalkOutcome reports the coarsest divergence there is: one runner
// finished the walk and the other did not.
//
// It is a graded surface rather than a fixture knob on purpose. An earlier
// draft declared "the engine is expected to error here" per case, which bakes
// today's broken behaviour into the fixture: when the port lands, the fixture's
// own expectation goes stale and the row fails for a reason that has nothing to
// do with the inventory row. Comparing the two walks instead means a port that
// makes the engine complete simply turns the row green.
func diffParityWalkOutcome(obs parityObservation) error {
	switch {
	case obs.Runner.Err != nil && obs.Engine.Err == nil:
		return fmt.Errorf("runner walk failed while the engine completed: %w", obs.Runner.Err)
	case obs.Engine.Err != nil && obs.Runner.Err == nil:
		return fmt.Errorf("engine walk failed while the runner completed: %w", obs.Engine.Err)
	}
	return nil
}

// checkAllSurfaces is the default row check: envelopes, then walk outcome, then
// journals, then the terminal. Envelopes come first because an envelope
// divergence explains most of the others, and reporting the cause beats
// reporting the symptom.
func checkAllSurfaces(obs parityObservation) error {
	if err := diffParityEnvelopes(obs); err != nil {
		return err
	}
	if err := diffParityWalkOutcome(obs); err != nil {
		return err
	}
	if err := diffConformanceViews(obs.Runner.Events, obs.Engine.Events); err != nil {
		return err
	}
	return diffParityTerminal(obs)
}

// checkParityCase runs the case's own check, defaulting to all three surfaces.
func checkParityCase(obs parityObservation) error {
	if obs.Case.Check != nil {
		return obs.Case.Check(obs)
	}
	return checkAllSurfaces(obs)
}

// assertParityHarnessIsNotVacuous fails the test outright (never gradeable as
// an expected failure) when a fixture proves nothing: no stage was dispatched,
// or a journal does not span run.started..run.finished. A row that stopped
// exercising anything must not be able to hide behind the expected-failure
// list.
func assertParityHarnessIsNotVacuous(t *testing.T, side paritySide) {
	t.Helper()
	if len(side.Envelopes) == 0 {
		t.Fatalf("%s side dispatched no stage — the fixture proves nothing", side.Name)
	}
	view := journal.ConformanceView(side.Events)
	if len(view) < 3 {
		t.Fatalf("%s conformance view has only %d events — the fixture proves nothing", side.Name, len(view))
	}
	if view[0].Type != journal.EventRunStarted {
		t.Fatalf("%s view does not begin at run.started: first=%s", side.Name, view[0].Type)
	}
	// A walk that errored closes no journal; that asymmetry is graded by
	// diffParityWalkOutcome, not fatal here.
	if side.Err == nil && view[len(view)-1].Type != journal.EventRunFinished {
		t.Fatalf("%s view does not end at run.finished: last=%s", side.Name, view[len(view)-1].Type)
	}
}

// --- the suite --------------------------------------------------------------

// TestParityRunnerVsEngine is the harness. Each registered row runs the same
// fixture through both runners and is graded against parityExpectedFailures in
// both directions.
func TestParityRunnerVsEngine(t *testing.T) {
	cases := parityCases()
	if len(cases) == 0 {
		t.Fatal("no parity rows registered — parity_row_*_test.go files are the seam and at least one must exist")
	}
	for i, c := range cases {
		t.Run(string(c.Row)+"/"+c.Name, func(t *testing.T) {
			if c.Build != nil {
				c.Build(t, &c)
			}
			runID := fmt.Sprintf("parity-%02d", i)
			obs := parityObservation{Case: c}
			obs.Runner = runParityRunnerSide(t, c, runID)
			obs.Engine = runParityEngineSide(t, c, runID)
			assertParityHarnessIsNotVacuous(t, obs.Runner)
			assertParityHarnessIsNotVacuous(t, obs.Engine)

			err := checkParityCase(obs)
			reason, expected := parityExpectedFailures[c.Row]
			lane := c.Lane
			if lane == "" {
				lane = "synthetic fixture"
			}
			switch {
			case err != nil && !expected:
				t.Fatalf("parity row %s (%s) FAILED and is not on the expected-failure list.\n"+
					"Either the port regressed, or this is a newly discovered inventory gap that needs a row in "+
					"findings/002 before it is added to parityExpectedFailures.\n%v", c.Row, lane, err)
			case err == nil && expected:
				t.Fatalf("parity row %s (%s) now PASSES but is still on the expected-failure list.\n"+
					"The port that closed it must ALSO delete its parityExpectedFailures entry.\nStale entry: %s", c.Row, lane, reason)
			case err != nil && expected:
				t.Logf("parity row %s (%s): expected failure, still open.\nreason: %s\ndiff: %v", c.Row, lane, reason, err)
			}
		})
	}
}

// TestParityExpectedFailuresAreRegisteredRows keeps the expected-failure list
// honest from the other end: an entry naming a row no case registers is dead
// weight that would silently absolve a deleted test.
func TestParityExpectedFailuresAreRegisteredRows(t *testing.T) {
	registered := map[parityRow]bool{}
	for _, c := range parityCases() {
		if registered[c.Row] {
			t.Errorf("parity row %s is registered more than once — a row id is the join key with parityExpectedFailures and must be unique", c.Row)
		}
		registered[c.Row] = true
	}
	for row, reason := range parityExpectedFailures {
		if !registered[row] {
			t.Errorf("parityExpectedFailures names row %s (%s) but no parity_row_*_test.go registers it — delete the entry or restore the case", row, reason)
		}
	}
}

// TestParityEnvelopeDiffNamesFirstDivergence is the harness self-test: an
// envelope divergence must be reported with its dispatch position and both
// sides printed. Without this the envelope surface could silently compare
// nothing (the #637 lesson, applied to the new surface).
func TestParityEnvelopeDiffNamesFirstDivergence(t *testing.T) {
	base := []parityEnvelope{
		{Stage: "query-backlog", Inputs: `curation="true"`},
		{Stage: "curate", Goober: "curator"},
	}
	same := parityObservation{
		Runner: paritySide{Name: "runner", Envelopes: base},
		Engine: paritySide{Name: "engine", Envelopes: base},
	}
	if err := diffParityEnvelopes(same); err != nil {
		t.Fatalf("identical envelope sequences must not diverge: %v", err)
	}

	divergent := append([]parityEnvelope(nil), base...)
	divergent[0].Inputs = `curation="true" requireLabels="goobers:cloud"`
	err := diffParityEnvelopes(parityObservation{
		Runner: paritySide{Name: "runner", Envelopes: divergent},
		Engine: paritySide{Name: "engine", Envelopes: base},
	})
	if err == nil {
		t.Fatal("divergent envelope sequences reported as identical")
	}
	for _, want := range []string{"dispatch 1", "requireLabels", "runner:", "engine:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("envelope diff output missing %q:\n%s", want, err)
		}
	}

	// A truncated sequence is reported with the extra dispatch, not accepted.
	err = diffParityEnvelopes(parityObservation{
		Runner: paritySide{Name: "runner", Envelopes: base},
		Engine: paritySide{Name: "engine", Envelopes: base[:1]},
	})
	if err == nil {
		t.Fatal("truncated envelope sequence reported as identical")
	}
	if !strings.Contains(err.Error(), "dispatch 2") || !strings.Contains(err.Error(), "curate") {
		t.Errorf("truncation diff output unhelpful: %v", err)
	}
}

// TestParityTerminalDiffNamesBothSides pins the terminal surface's own
// reporting, including the NoWork field the journal surface cannot see.
func TestParityTerminalDiffNamesBothSides(t *testing.T) {
	runnerSide := parityTerminal{Status: StatusCompleted, FinalState: "query-backlog", NoWork: true}
	engineSide := parityTerminal{Status: StatusCompleted, FinalState: "query-backlog"}
	if err := diffParityTerminal(parityObservation{
		Runner: paritySide{Terminal: runnerSide},
		Engine: paritySide{Terminal: runnerSide},
	}); err != nil {
		t.Fatalf("identical terminals must not diverge: %v", err)
	}
	err := diffParityTerminal(parityObservation{
		Runner: paritySide{Terminal: runnerSide},
		Engine: paritySide{Terminal: engineSide},
	})
	if err == nil {
		t.Fatal("divergent terminals reported as identical")
	}
	if !strings.Contains(err.Error(), "noWork=true") || !strings.Contains(err.Error(), "noWork=false") {
		t.Errorf("terminal diff output does not name the NoWork divergence: %v", err)
	}
}

// TestParityEnvelopeStringPrintsEveryComparedField keeps parityEnvelope's
// String in lockstep with its fields. Without it, adding a field to the
// comparison without adding it to String yields the useless failure message
// "these two identical-looking envelopes differ" — which is exactly what the
// first draft of this harness produced.
func TestParityEnvelopeStringPrintsEveryComparedField(t *testing.T) {
	// Every field set to a value that appears nowhere else in the rendering.
	full := parityEnvelope{
		Stage: "s-stage", WorkflowID: "s-workflow", Goal: "s-goal", Goober: "s-goober",
		Gaggle: "s-gaggle", BranchNamespace: "s-namespace", BaseBranch: "s-base",
		TriggerRef: "s-trigger", OwnershipBoundary: "s-ownership",
		MinimumIntegrity: apiv1.Integrity("s-integrity"),
		Inputs:           "s-inputs", Capabilities: "s-caps", PolicyActions: "s-policy",
		ContextPointers: "s-pointers", Item: "s-item",
	}
	rendered := full.String()
	for _, sentinel := range []string{
		"s-stage", "s-workflow", "s-goal", "s-goober", "s-gaggle", "s-namespace", "s-base",
		"s-trigger", "s-ownership", "s-integrity", "s-inputs", "s-caps", "s-policy", "s-pointers", "s-item",
	} {
		if !strings.Contains(rendered, sentinel) {
			t.Errorf("parityEnvelope.String() omits a compared field (%s):\n%s", sentinel, rendered)
		}
	}
	// Guard the other direction: a newly added field must be added to String.
	// reflect.NumField is the tripwire — bump the count deliberately, together
	// with the sentinel list above.
	if got, want := reflect.TypeOf(full).NumField(), 15; got != want {
		t.Fatalf("parityEnvelope now has %d fields, this test knows %d — add the new field to String() and to the sentinel list", got, want)
	}
}

// TestParityWalkOutcomeDiffNamesTheFailingSide pins the coarsest surface: a
// walk that failed on one side and not the other is reported, naming which
// side failed and why, and two clean (or two failed) walks are not.
func TestParityWalkOutcomeDiffNamesTheFailingSide(t *testing.T) {
	boom := errors.New("upstream output not found")
	clean := parityObservation{}
	if err := diffParityWalkOutcome(clean); err != nil {
		t.Fatalf("two clean walks must not diverge: %v", err)
	}
	both := parityObservation{
		Runner: paritySide{Name: "runner", Err: boom},
		Engine: paritySide{Name: "engine", Err: boom},
	}
	if err := diffParityWalkOutcome(both); err != nil {
		t.Fatalf("two failed walks are a fixture property, not a divergence: %v", err)
	}
	engineOnly := diffParityWalkOutcome(parityObservation{Engine: paritySide{Name: "engine", Err: boom}})
	if engineOnly == nil || !strings.Contains(engineOnly.Error(), "engine walk failed") {
		t.Errorf("engine-only failure not reported as such: %v", engineOnly)
	}
	if !errors.Is(engineOnly, boom) {
		t.Errorf("the reported divergence must wrap the walk's own error: %v", engineOnly)
	}
	runnerOnly := diffParityWalkOutcome(parityObservation{Runner: paritySide{Name: "runner", Err: boom}})
	if runnerOnly == nil || !strings.Contains(runnerOnly.Error(), "runner walk failed") {
		t.Errorf("runner-only failure not reported as such: %v", runnerOnly)
	}
}

// TestParityRowIDsAreDocumented guards the join key with finding 002: every
// registered row id must carry the inventory-row shape (a plan item or P0
// prefix and a descriptive slug), so a reader can find the row it pins.
func TestParityRowIDsAreDocumented(t *testing.T) {
	for _, c := range parityCases() {
		prefix, rest, ok := strings.Cut(string(c.Row), "-")
		if !ok || rest == "" {
			t.Errorf("parity row %q is not <planItem>-<slug>; the id is the join key with the finding-002 inventory", c.Row)
			continue
		}
		if prefix != "P0" && !strings.HasPrefix(prefix, "E") && !strings.HasPrefix(prefix, "D") && !strings.HasPrefix(prefix, "C") {
			t.Errorf("parity row %q does not name a finding-002 plan item (P0/E*/D*/C*)", c.Row)
		}
		if c.Build == nil && c.Spec.Start == "" {
			t.Errorf("parity row %q registers no fixture (set Spec at registration or Build to construct one)", c.Row)
		}
	}
}

// errParityRow wraps a row-check failure with the row id so a custom Check's
// message always says which inventory row it belongs to.
func errParityRow(row parityRow, format string, args ...any) error {
	return fmt.Errorf("row %s: %w", row, fmt.Errorf(format, args...))
}

// requireEnvelopeInput is the assertion most input-defaulting rows need: the
// named stage's envelope, on the named side, must carry input key=value.
func requireEnvelopeInput(side paritySide, stage, key, want string) error {
	for _, env := range side.Envelopes {
		if env.Stage != stage {
			continue
		}
		if strings.Contains(env.Inputs, fmt.Sprintf("%s=%q", key, want)) {
			return nil
		}
		return fmt.Errorf("%s envelope for stage %q lacks input %s=%q; inputs were: %s",
			side.Name, stage, key, want, env.Inputs)
	}
	if side.Err != nil {
		return fmt.Errorf("%s never dispatched stage %q; its walk ended with: %w", side.Name, stage, side.Err)
	}
	return fmt.Errorf("%s never dispatched stage %q", side.Name, stage)
}
