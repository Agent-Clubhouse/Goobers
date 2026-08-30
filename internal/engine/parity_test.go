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
// PREMISE VS CHECK — WHY THEY ARE TWO HOOKS. Every row needs an anti-vacuity
// assertion: the RUNNER side really does exhibit the behaviour the row exists
// to compare, so the row cannot pass with both sides equally wrong. That
// assertion must NOT be gradeable against parityExpectedFailures, and putting
// it at the top of Check (the harness's first shape) made it exactly that:
// a row on the expected-failure list swallowed its own premise failure into the
// "expected failure, still open" arm, so deleting the runner behaviour the row
// protects left this suite green. The premise is the reason to believe the
// row's diff means anything, so it is graded like assertParityHarnessIsNotVacuous
// — fatally, always — and only the DIVERGENCE is gradeable:
//
//   - Premise: does the runner still do the thing? Ungraded. Failing it is a
//     harness/fixture bug and fails the suite outright, on the list or not.
//   - Check:   do the two sides agree? Graded against parityExpectedFailures.
//
// Belt and braces: a premise assertion written inside Check by habit still
// cannot be swallowed — errParityPremisef tags the error and gradeParityRow
// re-raises anything carrying that tag (TestParityPremiseFailuresAreNotGradeable).
//
// SEAM FOR LATER PORTS. Each row lives in its own parity_row_<row>_test.go
// file and registers itself with registerParityRow in an init function. To add
// a failing-first case for a port:
//
//  1. add a parityRow constant naming the finding-002 inventory row and the
//     plan item that closes it (e.g. "E4-cached-verdict");
//  2. create parity_row_<slug>_test.go with an init that calls
//     registerParityRow, a Build (for a lane-derived fixture) or a literal
//     Spec, a Premise that asserts the RUNNER side really exhibits the
//     behaviour, and (optionally) a Check for the divergence — Premise is
//     MANDATORY and TestParityRowsDeclareAPremise enforces it;
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
	// rowBacklogQueryClaimPartition is the CONSEQUENCE half of the same
	// inventory row (plan item E1): the inputs the stage was handed, compiled
	// into the label filter backlog-query itself compiles, must reject the
	// SIBLING instance's goobers:local item. Input equality is what the port
	// changes; an unclaimable sibling item is what the partition means.
	rowBacklogQueryClaimPartition parityRow = "E1-backlog-query-claim-partition"
	// rowBacklogQueryDeclaredInputsWin is the OVER-APPLICATION half of the
	// same inventory row, and must stay GREEN in both directions: a stage that
	// declares its own requireLabels/assignedTo keeps them, before and after
	// the port. It is the row that says a blanket stamp is not a fix.
	rowBacklogQueryDeclaredInputsWin parityRow = "E1-backlog-query-declared-inputs-win"
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
	// rowNonRetryableEscalation is inventory row "#415 non-retryable
	// escalation bypass" (plan item E2): a failure with retryable=false and a
	// recognized escalate code routes to the Next gate's ESCALATE CONTROL
	// BRANCH without evaluating the gate.
	rowNonRetryableEscalation parityRow = "E2-nonretryable-escalation"
	// rowNonRetryableEscalationDefault is the same inventory row's default
	// arm: with no escalate control branch on the Next gate, the same failure
	// ends the run at @escalate rather than entering the repass loop.
	rowNonRetryableEscalationDefault parityRow = "E2-nonretryable-escalation-default"
	// rowRetryableFailureStillGated is the NEGATIVE half of the same row: a
	// RETRYABLE failure carrying the very same escalate code must still route
	// into the Next gate. It is what forbids a port that escalates on the code
	// alone.
	rowRetryableFailureStillGated parityRow = "E2-retryable-failure-still-gated"
	// rowUnrecognizedFailureStillGated is the other negative half: a
	// NON-retryable failure carrying a code the escalate set does not name
	// must also still route into the Next gate. It forbids a port keyed on
	// error.retryable alone.
	rowUnrecognizedFailureStillGated parityRow = "E2-unrecognized-failure-still-gated"
	// rowRetryDecisionAnnotation is inventory row "retry-decision annotation"
	// (plan item E2): a fail branch re-entering a completed stage leaves the
	// runner.annotation priorRepassCause reads back (E6).
	rowRetryDecisionAnnotation parityRow = "E2-retry-decision-annotation"
	// rowRetryDecisionNotOnPass is the negative half of the annotation row: a
	// PASSING gate, and an escalated repass, write no retry decision.
	rowRetryDecisionNotOnPass parityRow = "E2-retry-decision-not-on-pass"
	// rowPlacementProvenance is inventory row "runner.placement provenance"
	// (plan item E3, #3875): every stage attempt journals where it physically
	// executed, on both runners, once the deployment has declared placement.
	rowPlacementProvenance parityRow = "E3-placement-provenance"
	// rowCachedVerdictShortCircuit is inventory row "cached verdict reuse"
	// (plan item E4, #523): a gate whose subject hands forward a
	// digest-matched prior verdict routes on it WITHOUT dispatching a
	// reviewer.
	rowCachedVerdictShortCircuit parityRow = "E4-cached-verdict-short-circuit"
	// rowEmptyDiffFastFail is inventory row "#415 empty-diff fast-fail" (plan
	// item E5): an agentic stage that committed nothing fails its gate without
	// a reviewer, because an empty patch offers nothing to evaluate and a
	// repass can only reproduce it.
	rowEmptyDiffFastFail parityRow = "E5-empty-diff-fast-fail"
	// rowEmptyDiffDeterministicSubject is the SCOPE half of the same row, and
	// must stay GREEN in both directions: the identical empty diff over a
	// DETERMINISTIC subject is still reviewed. It is what forbids a port that
	// fast-fails on emptiness alone.
	rowEmptyDiffDeterministicSubject parityRow = "E5-empty-diff-deterministic-subject"
	// rowLearningEpisodeInjection is inventory row "learning-episode injection
	// on the generic gate retry arm" (plan item E10, #3913): a repassing gate
	// records the episode artifact, threads the derived learning.episode[<seq>]
	// pointer into the re-entered stage, writes the learning.episode.injected
	// annotation, and downgrades the repassed stage's produced integrity.
	//
	// The behaviour was ported by #3882/#3915. It had no inventory row of its
	// own at the time — which is why the retry-decision rows next door once
	// carried a documented surface exclusion rather than an expected-failure
	// entry: that map joins on inventory row id, and inventing one would have
	// corrupted the join key. E10 is that assigned id, and this row is where
	// the behaviour is pinned in its own right rather than as a side condition
	// of a row about something else.
	rowLearningEpisodeInjection parityRow = "E10-learning-episode-injection"
	// rowLearningEpisodeNotInjected is the NEGATIVE half of the same row: a
	// gate whose fail branch routes ONWARD, to a stage that has not run, is not
	// a repass — there is nothing to correct and no attempt to feed. It is what
	// forbids an injection keyed on "the gate failed" rather than on "the gate
	// sent a stage back", which would fill forward stages' envelopes with
	// derived pointers and downgrade stages that were never repassed.
	rowLearningEpisodeNotInjected parityRow = "E10-learning-episode-not-injected"
	// rowLearningEpisodeContextFrom is the sub-case #3928 discovered in the
	// E10 ruling review: the injected pointer must survive the RE-ENTERED
	// STAGE'S OWN contextFrom selection. Every other E10 fixture declares no
	// contextFrom, which is the one configuration where the selector cannot
	// drop anything — while the flagship implementation lane, the lane the
	// injection exists for, does declare one. Both runners called the same
	// shared selector, so the divergence was zero and the behaviour was
	// absent: this row's premise, not its check, is what pins the fix.
	rowLearningEpisodeContextFrom parityRow = "E10-learning-episode-contextfrom"
	// rowLearningEpisodeForwardBranch is the sub-case this table DISCOVERED
	// while pinning the two above: a gate's fail branch that routes ONWARD, to
	// a stage that has not run, on a RETRY-CLASSIFIABLE failure. It is the one
	// place the two sides disagreed — the local runner injected there
	// (routeRetryDecision asks only retryable/non-pass/non-escalated/
	// real-target), the engine did not — and it was registered as a documented
	// expected failure because resolving it was a ruling, not a patch.
	//
	// #3929 took that ruling: an episode is injected IFF the branch is a true
	// repass, using the gate result's own repassAttempt. So the row is now the
	// POSITIVE statement of the ruling rather than a report of a divergence —
	// neither side injects, and the expected-failure entry is gone with it.
	//
	// It is deliberately distinct from rowLearningEpisodeNotInjected, which
	// also routes onward: there the retry classifier DECLINES the failure, so
	// there is no retry arm at all and the row would stay green under either
	// side of the ruling. Here the classifier ACCEPTS it, the retry decision is
	// really taken and really annotated, and only the injection is withheld.
	// That is the distinction the ruling is about.
	rowLearningEpisodeForwardBranch parityRow = "E10-learning-episode-forward-branch"
	// rowLearningEpisodeInfraForwardBranch is the same ruling on the arm that
	// actually carries it in production, and on the failure class the synthetic
	// row cannot reach.
	//
	// The synthetic forward-branch row above walks a status-equals gate over a
	// nonzero_exit failure — journal.AttemptPolicy. The only shipped branch the
	// ruling CHANGES is pr-remediation's `local-gate` (failure-class evaluator,
	// `infra: park-infrastructure-failure`), which is retryable by the OTHER
	// route through retryFailureClassForGateResult: not because the failure
	// code is recognized, but because the gate resolved gate.OutcomeInfra. That
	// is journal.AttemptInfra, a different class down a different branch of the
	// classifier, and no other row in this table produces it.
	//
	// The target is a parking stage whose own escalation text says "no
	// implementation defect was established" — the concrete case for the
	// ruling, and the reason this row is shaped like production rather than
	// like a fixture.
	rowLearningEpisodeInfraForwardBranch parityRow = "E10-learning-episode-infra-forward-branch"
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
//
// The list is EMPTY again as of plan item E10's ruling (#3929), which closed
// the E10-learning-episode-forward-branch entry #3917 registered — the only one
// it has ever carried since E1 (#3873) and E2 (#3874) closed the backlog-query
// defaulting, NoWork and stage-qualified inputsFrom rows. An empty list is the
// strongest state this harness can be in — every registered row is green on
// both runners — and it is not a licence to stop adding rows. A gap the
// inventory names but no row pins is still a gap; see the drift ledger in
// doc.go for divergences observed but not yet inventoried, and #3928/#3930/
// #3931/#3932 for defects that are real but are NOT parity divergences,
// because both runners share them.
var parityExpectedFailures = map[parityRow]string{}

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
	// Verdicts scripts the AGENTIC REVIEWER per gate name (#3882). A gate
	// absent from this map is one the row asserts is never dispatched: the
	// scripted reviewer fails loudly if it is reached, so an over-eager port
	// cannot pass by producing the right outcome through an extra model call.
	Verdicts map[string][]apiv1.Verdict
	// EngineWorkspaceDiffs scripts what the ENGINE side's workspace reports
	// from DiffReader, keyed by stage.
	//
	// It is engine-side only, and deliberately so. The local runner reads a
	// real git worktree, so its diff is whatever the fixture's scripted stages
	// actually left behind — which, since scriptedExec never writes files, is
	// nothing. A row comparing diff-derived behaviour therefore scripts the
	// engine to report the SAME nothing, and what is being compared is the two
	// lanes' DECISIONS given the same observation. Scripting a non-empty diff
	// here would compare a real observation against a fictional one.
	EngineWorkspaceDiffs map[string][]byte
	// RunControls is the run's already-resolved run-control policy, threaded
	// through the SAME seam production uses on each side: runner.StartInput's
	// RunControls field and engine.StartSpec's, which Registry.StartInputVersion
	// pins into RunInput. Deliberately NOT runner.Config.MaxRepasses /
	// RunInput.MaxRepasses — both are documented-deprecated compatibility
	// fields, and a harness that pins policy through them proves nothing about
	// the path a production run takes.
	RunControls apiv1.RunControls
	// BacklogQueryAssignedTo / BacklogQueryRequireLabels are the gaggle's
	// claim-partition defaults (MIRC-2), threaded through the seam each side
	// takes in production: runner.Config for the local runner, and
	// engine.StartSpec — pinned into RunInput by Registry.StartInputVersion —
	// for the engine (#3873, plan item E1).
	BacklogQueryAssignedTo    string
	BacklogQueryRequireLabels string
	// UsesRepo marks a fixture whose stages take a repo workspace, so the
	// local side needs the hermetic git fixture repo.
	UsesRepo bool
	// Premise is the row's ANTI-VACUITY assertion, and it is MANDATORY
	// (TestParityRowsDeclareAPremise). It asserts that the RUNNER side really
	// exhibits the behaviour this row exists to compare — "the local runner
	// still reports NoWork for a step-1 no-work terminal", "the runner still
	// defaults requireLabels onto this stage".
	//
	// It runs UNGRADED: a premise failure fails the test outright even for a
	// row on parityExpectedFailures, exactly like
	// assertParityHarnessIsNotVacuous. That is the whole point — a row whose
	// premise has stopped holding is not "a known gap, still open", it is a
	// fixture that has stopped testing anything, and the expected-failure list
	// must never be able to absorb it.
	Premise func(obs parityObservation) error
	// Check is the row's DIVERGENCE assertion: do the two sides agree? It
	// returns an error rather than calling t.Fatal because this is the one
	// thing parityExpectedFailures may grade. Nil means "the default surfaces
	// must match" (checkAllSurfaces).
	//
	// Do not put a runner-side premise assertion here — that is Premise's job,
	// and a graded premise is the bug this split exists to prevent. If you do
	// anyway, raise it with errParityPremisef so the grader re-raises it.
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
// The exclusion set is exhaustive and TESTED: every apiv1.InvocationEnvelope
// field is either projected below or named in parityEnvelopeExcludedFields, and
// TestParityEnvelopeComparesEveryEnvelopeField fails when a new envelope field
// is neither. Without that second tripwire "everything else is normative" was
// aspirational — eight fields (RunID, InstructionAddendum, RepoRef,
// AdditionalWorkspaces, CheckoutCones, ParentPlatformPolicy, NestedAgentPolicy
// and Limits) were silently dropped, Limits and the two nested-agent policies
// most pointedly: they are the safety knobs R9 admits maxDurationSeconds on the
// argument that the engine enforces it via StartToCloseTimeout, and this harness
// is what is supposed to prove both sides pass the same value.
//
// Excluded, with the reason:
//   - Workspace: an absolute path minted per attempt by each side's
//     provisioner (runner: worktree.Manager; engine: the activity's
//     WorkspaceProvisioner). Never the same string, never can be.
//   - Attempt: infra retries renumber attempts independently on each side;
//     journal.ConformanceView excludes infra-retry attempts for the same
//     reason. Dispatch ORDER is preserved by the slice itself.
//   - AdditionalWorkspaces[i].Path: an absolute checkout path, excluded for
//     exactly the Workspace reason. The reference repo's NAME is the part a
//     stage's cross-repo context depends on, and it IS compared.
//   - ContextPointers' Artifact.Path/Digest/Size: journal-relative locations
//     and content digests of bytes each side wrote itself. The pointer NAME
//     and Integrity grade are what a stage's admission and evidence depend
//     on, and both are compared (ContextPointers below).
type parityEnvelope struct {
	Stage      string
	RunID      string
	WorkflowID string
	Goal       string
	Goober     string
	// GooberDigest is the kit the run was admitted against, and since #3884
	// the value the worker SELECTS its kit by. Both drivers stamp it from the
	// run's pin, so a side that dropped it — or minted its own — would be
	// dispatching against a kit the other side never agreed to.
	GooberDigest      string
	Gaggle            string
	BranchNamespace   string
	BaseBranch        string
	TriggerRef        string
	OwnershipBoundary string
	MinimumIntegrity  apiv1.Integrity
	// InstructionAddendum is operator-supplied one-off instruction text. It is
	// empty for every ordinary invocation, so a side that started injecting it
	// is a divergence worth failing on.
	InstructionAddendum string
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
	// RepoRef is the target repo as it rides the wire (RepoRef.EnvelopeRef),
	// JSON-encoded. A stage that is handed the wrong repo, owner or base
	// branch is the bluntest parity bug there is.
	RepoRef string
	// AdditionalWorkspaces encodes the read-only reference-repo checkouts by
	// NAME, in order (paths excluded — see above).
	AdditionalWorkspaces string
	// CheckoutCones encodes the sparse-checkout cones per workspace (#649),
	// key-sorted. A side that materializes a different slice of the tree hands
	// its stage a different repo.
	CheckoutCones string
	// Limits is the stage's execution bound (duration/tokens/cost),
	// JSON-encoded. R9 refuses maxTokens/maxCostUSD at run start and admits
	// maxDurationSeconds precisely BECAUSE the engine enforces it through the
	// stage activity's StartToCloseTimeout — so the two sides passing the same
	// Limits is the evidence that admission rests on.
	Limits string
	// ParentPlatformPolicy and NestedAgentPolicy are the runner-authored child
	// authority for agentic stages, JSON-encoded. They are carried in the
	// envelope exactly so an adapter cannot infer nested-agent authority from
	// prompt text; a side that drops or widens them is a privilege divergence.
	ParentPlatformPolicy string
	NestedAgentPolicy    string
}

// parityEnvelopeExcludedFields names every apiv1.InvocationEnvelope field this
// projection deliberately does NOT compare, with the reason recorded in
// parityEnvelope's doc comment above.
// TestParityEnvelopeComparesEveryEnvelopeField asserts this set plus the
// projected fields covers the envelope exactly, so a new envelope field cannot
// join the exclusion set by being forgotten.
var parityEnvelopeExcludedFields = map[string]string{
	"Workspace": "absolute path minted per attempt by each side's provisioner; never the same string",
	"Attempt":   "infra retries renumber attempts independently on each side; dispatch order is the slice's",
}

// String prints EVERY compared field. A projection that compares a field it
// does not print produces the worst possible failure message — "these two
// identical-looking envelopes differ" — so the two must stay in lockstep;
// TestParityEnvelopeStringPrintsEveryComparedField enforces that.
func (e parityEnvelope) String() string {
	return fmt.Sprintf("stage=%s runId=%s workflowId=%s gaggle=%s goal=%q goober=%s gooberDigest=%s ownership=%s "+
		"branchNamespace=%q baseBranch=%q triggerRef=%q minIntegrity=%q addendum=%q "+
		"inputs=[%s] caps=[%s] policy=[%s] pointers=[%s] item=%q "+
		"repoRef=%s additionalWorkspaces=[%s] checkoutCones=%s limits=%s parentPlatformPolicy=%s nestedAgentPolicy=%s",
		e.Stage, e.RunID, e.WorkflowID, e.Gaggle, e.Goal, e.Goober, e.GooberDigest, e.OwnershipBoundary,
		e.BranchNamespace, e.BaseBranch, e.TriggerRef, e.MinimumIntegrity, e.InstructionAddendum,
		e.Inputs, e.Capabilities, e.PolicyActions, e.ContextPointers, e.Item,
		e.RepoRef, e.AdditionalWorkspaces, e.CheckoutCones, e.Limits,
		e.ParentPlatformPolicy, e.NestedAgentPolicy)
}

// stageOf extracts the stage name from an envelope TaskID ("<runID>:<stage>"),
// the same split the scripted executors use.
func stageOf(taskID string) string {
	return taskID[strings.Index(taskID, ":")+1:]
}

func projectParityEnvelope(env apiv1.InvocationEnvelope) parityEnvelope {
	return parityEnvelope{
		Stage:                stageOf(env.TaskID),
		RunID:                env.RunID,
		WorkflowID:           env.WorkflowID,
		Goal:                 env.Goal,
		Goober:               env.Goober,
		GooberDigest:         env.GooberDigest,
		Gaggle:               env.Gaggle,
		BranchNamespace:      env.BranchNamespace,
		BaseBranch:           env.BaseBranch,
		TriggerRef:           env.TriggerRef,
		OwnershipBoundary:    env.OwnershipBoundary,
		MinimumIntegrity:     env.MinimumIntegrity,
		InstructionAddendum:  env.InstructionAddendum,
		Inputs:               encodeParityInputs(env.Inputs),
		Capabilities:         strings.Join(env.Capabilities, ","),
		PolicyActions:        strings.Join(env.PolicyActions, ","),
		ContextPointers:      encodeParityPointers(env.ContextPointers),
		Item:                 encodeParityItem(env.Item),
		RepoRef:              encodeParityJSON(env.RepoRef),
		AdditionalWorkspaces: encodeParityAdditionalWorkspaces(env.AdditionalWorkspaces),
		CheckoutCones:        encodeParityJSON(env.CheckoutCones),
		Limits:               encodeParityJSON(env.Limits),
		ParentPlatformPolicy: encodeParityJSON(env.ParentPlatformPolicy),
		NestedAgentPolicy:    encodeParityJSON(env.NestedAgentPolicy),
	}
}

// encodeParityJSON renders a structured envelope field as canonical JSON: the
// struct's own field order, map keys sorted by encoding/json, so the encoding is
// stable across sides and runs. It is used for the fields whose Go types are not
// comparable with == (maps, pointers) or are clearer read as JSON in a failure
// message than as %v.
func encodeParityJSON(v interface{}) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		// Deliberately not fatal: this is a projection helper with no *testing.T,
		// and a marshal failure must still produce a value that DIFFERS from a
		// clean encoding rather than silently comparing equal to one.
		return fmt.Sprintf("<unencodable %T: %v>", v, err)
	}
	return string(encoded)
}

// encodeParityAdditionalWorkspaces renders the read-only reference-repo
// checkouts by name, in order. Path is excluded for the same reason Workspace
// is: it is an absolute path each side's provisioner mints for itself.
func encodeParityAdditionalWorkspaces(workspaces []apiv1.AdditionalWorkspace) string {
	if len(workspaces) == 0 {
		return ""
	}
	names := make([]string, 0, len(workspaces))
	for _, w := range workspaces {
		names = append(names, w.Name)
	}
	return strings.Join(names, ",")
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

// newRecordingExecForCase wires both scripted seams from the row.
func newRecordingExecForCase(c parityCase) *recordingExec {
	exec := newRecordingExec(c.Script)
	exec.inner.verdicts = c.Verdicts
	return exec
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

// Review records the reviewer's envelope alongside the stage envelopes, so a
// row can assert on what the REVIEWER was handed — the "<gate>.diff" pointer
// (#3384) is only observable here — and, by its absence, that no reviewer ran
// at all.
func (r *recordingExec) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	r.record(env)
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
	env = b.stamp(env)
	b.owner.record(env)
	return b.owner.inner.Review(ctx, env)
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
	exec := newRecordingExecForCase(c)
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
		Automated: gate.NewAutomatedEvaluator(),
		// Config.MaxRepasses is deprecated in favour of per-run
		// StartInput.RunControls (run.go:414) and is deliberately NOT set: the
		// run's policy is pinned below through the field production dispatch
		// uses, so this harness compares the live seam rather than a
		// compatibility one.
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
		RunID:       runID,
		Machine:     machine,
		Gaggle:      c.Spec.Gaggle,
		Trigger:     journal.Trigger{Kind: journal.TriggerManual},
		RepoRef:     parityRepoRef,
		RunControls: c.RunControls,
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
// parityEngineRunInput builds the engine side's RunInput the way PRODUCTION
// builds it: register the fixture in a Registry and pin it through
// StartInputVersion, rather than hand-filling the struct.
//
// Going through the registry is the point, not ceremony. StartInputVersion is
// where preview-feature acknowledgement is pinned, where RunControls is pinned,
// and where this PR's own R9 refusal lives — a hand-built RunInput routes around
// all three, so a future parity row for a lane declaring spec.parallels would
// have WALKED here while production refuses it at start. Building through the
// registry means the compared engine path is the started path.
func parityEngineRunInput(t *testing.T, c parityCase, runID string) RunInput {
	t.Helper()
	reg := NewRegistryWithPreviewFeatures(true)
	version, err := reg.RegisterDefinition(parityDefinition(c))
	if err != nil {
		t.Fatalf("register fixture for row %s: %v", c.Row, err)
	}
	in, err := reg.StartInputVersion(parityWorkflowName, version, StartSpec{
		RunID:                     runID,
		Gaggle:                    c.Spec.Gaggle,
		RepoRef:                   parityRepoRef,
		TriggerKind:               string(journal.TriggerManual),
		BranchNamespace:           parityBranchNamespace,
		RunControls:               c.RunControls,
		BacklogQueryAssignedTo:    c.BacklogQueryAssignedTo,
		BacklogQueryRequireLabels: c.BacklogQueryRequireLabels,
	})
	if err != nil {
		t.Fatalf("pin start input for row %s: %v", c.Row, err)
	}
	return in
}

func runParityEngineSide(t *testing.T, c parityCase, runID string) paritySide {
	t.Helper()
	exec := newRecordingExecForCase(c)
	in := parityEngineRunInput(t, c, runID)
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.SetStartTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	workspaces := testWorkspaces(t)
	for stage, diff := range c.EngineWorkspaceDiffs {
		workspaces.scriptDiff(stage, diff)
	}
	env.RegisterActivity(&Activities{
		Goober:     exec,
		Det:        exec,
		Auto:       gate.NewAutomatedEvaluator(),
		Workspaces: workspaces,
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

// errParityPremise tags an ANTI-VACUITY failure — "the runner side no longer
// exhibits the behaviour this row exists to compare" — as ungradeable.
//
// The tag exists because the premise and the divergence are both reported as a
// plain error, and only the divergence may be graded against
// parityExpectedFailures. A row's Premise hook is raised through
// errParityPremisef, and gradeParityRow re-raises anything carrying this tag
// even when the row is on the expected-failure list, so a premise assertion
// written inside Check by habit still cannot be swallowed.
var errParityPremise = errors.New("parity premise no longer holds")

// errParityPremisef builds a tagged premise failure naming the row.
func errParityPremisef(row parityRow, format string, args ...any) error {
	return fmt.Errorf("row %s: %w: %w", row, errParityPremise, fmt.Errorf(format, args...))
}

// parityGrade is how one row's outcome is classified against
// parityExpectedFailures.
type parityGrade int

const (
	// parityGradeAgreed: the two sides agree and the row is not on the list.
	parityGradeAgreed parityGrade = iota
	// parityGradeRegression: the row diverges and is NOT on the list.
	parityGradeRegression
	// parityGradeStaleExpectation: the row agrees but IS still on the list —
	// the port landed and its entry must be deleted.
	parityGradeStaleExpectation
	// parityGradeExpectedFailure: the row diverges and is on the list.
	parityGradeExpectedFailure
	// parityGradeVacuous: the row's own premise failed. NEVER gradeable — the
	// fixture has stopped exercising the behaviour, which the expected-failure
	// list must not be able to absorb.
	parityGradeVacuous
)

// gradeParityRow classifies a row's check outcome. It is a pure function of
// (error, on-the-list) so the grading policy itself is unit-testable —
// TestParityPremiseFailuresAreNotGradeable is the test the mustFix this split
// answers would have failed.
func gradeParityRow(err error, expected bool) parityGrade {
	switch {
	case errors.Is(err, errParityPremise):
		// Checked FIRST and independently of `expected`: a vacuous fixture is
		// never a "known gap, still open".
		return parityGradeVacuous
	case err != nil && !expected:
		return parityGradeRegression
	case err == nil && expected:
		return parityGradeStaleExpectation
	case err != nil:
		return parityGradeExpectedFailure
	default:
		return parityGradeAgreed
	}
}

// TestParityRunnerVsEngine is the harness. Each registered row runs the same
// fixture through both runners, has its ungraded premise checked, and is then
// graded against parityExpectedFailures in both directions.
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

			lane := c.Lane
			if lane == "" {
				lane = "synthetic fixture"
			}

			// The premise runs BEFORE the graded check and fails outright. A
			// row that has stopped exercising its behaviour proves nothing, on
			// the expected-failure list or not.
			if c.Premise == nil {
				t.Fatalf("parity row %s (%s) declares no Premise — every row must assert that the runner "+
					"side still exhibits the behaviour it compares, or the row can pass with both sides equally wrong", c.Row, lane)
			}
			if err := c.Premise(obs); err != nil {
				t.Fatalf("parity row %s (%s): the fixture no longer exercises the behaviour this row pins.\n"+
					"This is NOT gradeable against parityExpectedFailures: a vacuous row is a harness bug, "+
					"not a known gap.\n%v", c.Row, lane, err)
			}

			err := checkParityCase(obs)
			reason, expected := parityExpectedFailures[c.Row]
			switch gradeParityRow(err, expected) {
			case parityGradeVacuous:
				t.Fatalf("parity row %s (%s) reported a PREMISE failure from its Check.\n"+
					"A premise assertion belongs in Premise, where it runs ungraded; it is re-raised here "+
					"rather than graded against parityExpectedFailures.\n%v", c.Row, lane, err)
			case parityGradeRegression:
				t.Fatalf("parity row %s (%s) FAILED and is not on the expected-failure list.\n"+
					"Either the port regressed, or this is a newly discovered inventory gap that needs a row in "+
					"findings/002 before it is added to parityExpectedFailures.\n%v", c.Row, lane, err)
			case parityGradeStaleExpectation:
				t.Fatalf("parity row %s (%s) now PASSES but is still on the expected-failure list.\n"+
					"The port that closed it must ALSO delete its parityExpectedFailures entry.\nStale entry: %s", c.Row, lane, reason)
			case parityGradeExpectedFailure:
				t.Logf("parity row %s (%s): expected failure, still open.\nreason: %s\ndiff: %v", c.Row, lane, reason, err)
			}
		})
	}
}

// TestParityPremiseFailuresAreNotGradeable is the regression test for the hole
// this harness shipped with: a row's own anti-vacuity assertion returned a plain
// error, the grader ran every error through parityExpectedFailures, and so the
// premise was inert for exactly the rows it exists to protect — deleting the
// runner behaviour a listed row pins left the suite green.
//
// It pins the policy, not one row: a premise-tagged error is ungradeable in
// BOTH directions, and an ordinary divergence still grades normally.
func TestParityPremiseFailuresAreNotGradeable(t *testing.T) {
	premise := errParityPremisef(rowRunResultNoWork, "runner did not report NoWork (%s)", "noWork=false")
	divergence := errors.New("stage envelopes diverge at dispatch 1")

	for _, expected := range []bool{true, false} {
		if got := gradeParityRow(premise, expected); got != parityGradeVacuous {
			t.Errorf("a premise failure with expected=%t graded %v, want parityGradeVacuous — "+
				"the expected-failure list must never absorb a vacuous fixture", expected, got)
		}
		// A premise failure wrapped further up (an errParityRow around it, say)
		// must still be recognised: the grader classifies with errors.Is.
		wrapped := fmt.Errorf("row %s: %w", rowRunResultNoWork, premise)
		if got := gradeParityRow(wrapped, expected); got != parityGradeVacuous {
			t.Errorf("a WRAPPED premise failure with expected=%t graded %v, want parityGradeVacuous", expected, got)
		}
	}

	if got := gradeParityRow(divergence, true); got != parityGradeExpectedFailure {
		t.Errorf("an ordinary divergence on the list graded %v, want parityGradeExpectedFailure", got)
	}
	if got := gradeParityRow(divergence, false); got != parityGradeRegression {
		t.Errorf("an ordinary divergence off the list graded %v, want parityGradeRegression", got)
	}
	if got := gradeParityRow(nil, true); got != parityGradeStaleExpectation {
		t.Errorf("a passing row still on the list graded %v, want parityGradeStaleExpectation", got)
	}
	if got := gradeParityRow(nil, false); got != parityGradeAgreed {
		t.Errorf("a passing row off the list graded %v, want parityGradeAgreed", got)
	}

	// The tagged error must still READ as the row's own failure: the grader
	// prints it verbatim.
	if !strings.Contains(premise.Error(), string(rowRunResultNoWork)) || !strings.Contains(premise.Error(), "noWork=false") {
		t.Errorf("premise failure loses the row id or the observed value: %v", premise)
	}
}

// TestParityRowsDeclareAPremise makes the anti-vacuity assertion structural
// rather than a convention a future port can forget. A row without one can pass
// with both sides equally wrong — and for a row on parityExpectedFailures, which
// is failing-first by design, nothing else would ever notice.
func TestParityRowsDeclareAPremise(t *testing.T) {
	for _, c := range parityCases() {
		if c.Premise == nil {
			t.Errorf("parity row %s (%s) declares no Premise; see the recipe at the top of parity_test.go — "+
				"assert that the RUNNER side still exhibits the behaviour before the row's diff is believed", c.Row, c.Name)
		}
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
		Stage: "s-stage", RunID: "s-runid", WorkflowID: "s-workflow", Goal: "s-goal", Goober: "s-goober",
		GooberDigest: "s-gooberdigest",
		Gaggle:       "s-gaggle", BranchNamespace: "s-namespace", BaseBranch: "s-base",
		TriggerRef: "s-trigger", OwnershipBoundary: "s-ownership",
		MinimumIntegrity:    apiv1.Integrity("s-integrity"),
		InstructionAddendum: "s-addendum",
		Inputs:              "s-inputs", Capabilities: "s-caps", PolicyActions: "s-policy",
		ContextPointers: "s-pointers", Item: "s-item",
		RepoRef: "s-reporef", AdditionalWorkspaces: "s-additional", CheckoutCones: "s-cones",
		Limits: "s-limits", ParentPlatformPolicy: "s-parentpolicy", NestedAgentPolicy: "s-nestedpolicy",
	}
	rendered := full.String()
	for _, sentinel := range []string{
		"s-stage", "s-runid", "s-workflow", "s-goal", "s-goober", "s-gooberdigest", "s-gaggle", "s-namespace", "s-base",
		"s-trigger", "s-ownership", "s-integrity", "s-addendum", "s-inputs", "s-caps", "s-policy",
		"s-pointers", "s-item", "s-reporef", "s-additional", "s-cones", "s-limits",
		"s-parentpolicy", "s-nestedpolicy",
	} {
		if !strings.Contains(rendered, sentinel) {
			t.Errorf("parityEnvelope.String() omits a compared field (%s):\n%s", sentinel, rendered)
		}
	}
	// Guard the other direction: a newly added field must be added to String.
	// reflect.NumField is the tripwire — bump the count deliberately, together
	// with the sentinel list above.
	if got, want := reflect.TypeOf(full).NumField(), 24; got != want {
		t.Fatalf("parityEnvelope now has %d fields, this test knows %d — add the new field to String() and to the sentinel list", got, want)
	}
}

// TestParityEnvelopeComparesEveryEnvelopeField is the tripwire the projection
// was missing. TestParityEnvelopeStringPrintsEveryComparedField guards
// parityEnvelope's OWN field count, which says nothing about the type it
// projects — so a new apiv1.InvocationEnvelope field joined the silent-exclusion
// set with no test firing, and eight already had.
//
// This test asserts the partition is total and deliberate: every envelope field
// is either projected by projectParityEnvelope or listed, with a reason, in
// parityEnvelopeExcludedFields. Adding a field to the envelope fails here until
// someone decides which side of the line it falls on.
func TestParityEnvelopeComparesEveryEnvelopeField(t *testing.T) {
	// Fields whose envelope name differs from the parityEnvelope field that
	// carries them.
	renamed := map[string]string{
		"TaskID": "Stage", // projected as the stage name via stageOf
	}
	projected := map[string]bool{}
	projectedType := reflect.TypeOf(parityEnvelope{})
	for i := 0; i < projectedType.NumField(); i++ {
		projected[projectedType.Field(i).Name] = true
	}

	envelopeType := reflect.TypeOf(apiv1.InvocationEnvelope{})
	for i := 0; i < envelopeType.NumField(); i++ {
		name := envelopeType.Field(i).Name
		carrier := name
		if alias, ok := renamed[name]; ok {
			carrier = alias
		}
		_, excluded := parityEnvelopeExcludedFields[name]
		if projected[carrier] == excluded {
			if excluded {
				t.Errorf("apiv1.InvocationEnvelope.%s is BOTH projected and listed as excluded — "+
					"parityEnvelopeExcludedFields must name only fields the projection drops", name)
				continue
			}
			t.Errorf("apiv1.InvocationEnvelope.%s is neither compared by projectParityEnvelope nor named in "+
				"parityEnvelopeExcludedFields.\nThe harness header claims everything outside that list is normative, so "+
				"either project the field or record why it cannot agree between a local worktree and a worker workspace.", name)
		}
	}

	// And the exclusion list may not name a field the envelope no longer has:
	// a stale exclusion silently re-opens the hole it documented.
	for name := range parityEnvelopeExcludedFields {
		if _, ok := envelopeType.FieldByName(name); !ok {
			t.Errorf("parityEnvelopeExcludedFields names %q, which apiv1.InvocationEnvelope no longer declares — delete the entry", name)
		}
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

// TestParityRowIDsAreWellFormed checks the SHAPE of the join key with finding
// 002: a plan-item-or-P0 prefix and a descriptive slug, so a reader can find the
// inventory row an id pins.
//
// Named for what it does. It was TestParityRowIDsAreDocumented, which promised
// more than it delivers — nothing here reads the inventory, so "Explosion-foo"
// passes. Genuinely enforcing the join would mean reading
// findings/002-pivot-plan-parity-inventory.md, which lives in the review tree
// and not in this repo, so it is not available to CI; the honest fix is the
// accurate name.
func TestParityRowIDsAreWellFormed(t *testing.T) {
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

// requireEnvelopeInput is the assertion most input-defaulting rows need: EVERY
// envelope the named side dispatched for the named stage must carry input
// key=value, and it must have dispatched the stage at least once.
//
// "Every", not "the first". The helper originally returned on the first
// matching envelope, which reads as "this stage's envelope carries X" but means
// "its first dispatch did" — and a stage is dispatched more than once as soon as
// a row exercises a retry or a repass re-entry, which is exactly where defaulting
// is most likely to be applied on the way in and lost on the way back round.
// Asserting every dispatch costs nothing today (no row repasses yet) and keeps
// the helper honest when one does.
func requireEnvelopeInput(side paritySide, stage, key, want string) error {
	dispatches := 0
	for _, env := range side.Envelopes {
		if env.Stage != stage {
			continue
		}
		dispatches++
		if !strings.Contains(env.Inputs, fmt.Sprintf("%s=%q", key, want)) {
			return fmt.Errorf("%s envelope for stage %q (dispatch %d) lacks input %s=%q; inputs were: %s",
				side.Name, stage, dispatches, key, want, env.Inputs)
		}
	}
	if dispatches > 0 {
		return nil
	}
	if side.Err != nil {
		return fmt.Errorf("%s never dispatched stage %q; its walk ended with: %w", side.Name, stage, side.Err)
	}
	return fmt.Errorf("%s never dispatched stage %q", side.Name, stage)
}

// requireEveryTaskDispatched asserts the side dispatched every task the fixture
// declares. It is the whole-lane premise: derived from the spec rather than a
// hard-coded stage list, so a lane that legitimately reorders or renames its
// stages still asserts "the walk reached all of them", while a lane that
// short-circuits after its first stage — leaving both sides agreeing about
// almost nothing — fails.
func requireEveryTaskDispatched(side paritySide, spec apiv1.WorkflowSpec) error {
	dispatched := map[string]int{}
	for _, env := range side.Envelopes {
		dispatched[env.Stage]++
	}
	missing := make([]string, 0, len(spec.Tasks))
	for _, task := range spec.Tasks {
		if dispatched[task.Name] == 0 {
			missing = append(missing, task.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s never dispatched %d of the lane's %d stage(s): %s (it dispatched %d envelope(s))",
			side.Name, len(missing), len(spec.Tasks), strings.Join(missing, ", "), len(side.Envelopes))
	}
	return nil
}

// requireStagesDispatched asserts the side dispatched exactly the named stages,
// in order. It is the anti-vacuity premise for a whole-lane row, where "the
// fixture still walks the lane" is the entire claim: assertParityHarnessIsNotVacuous
// only knows that SOME stage ran, so a lane that silently stopped short after its
// first stage would otherwise have both sides agreeing about very little.
func requireStagesDispatched(side paritySide, stages []string) error {
	got := make([]string, 0, len(side.Envelopes))
	for _, env := range side.Envelopes {
		got = append(got, env.Stage)
	}
	if len(got) != len(stages) {
		return fmt.Errorf("%s dispatched %d stage(s) [%s], want %d [%s]",
			side.Name, len(got), strings.Join(got, " "), len(stages), strings.Join(stages, " "))
	}
	for i := range stages {
		if got[i] != stages[i] {
			return fmt.Errorf("%s dispatch %d was stage %q, want %q; full order was [%s]",
				side.Name, i+1, got[i], stages[i], strings.Join(got, " "))
		}
	}
	return nil
}
