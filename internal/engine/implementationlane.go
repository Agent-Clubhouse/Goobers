package engine

import (
	"encoding/json"
	"fmt"

	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
)

// This file carries the implementation-lane behaviours ported from
// internal/runner under issue #3882 (decision 005, plan items E4–E9). Every
// one of them was runner-only, which meant a workflow driven by the engine
// reached a reviewer with less evidence, salvaged less work, and learned
// nothing across repasses compared with the identical workflow driven
// locally. The finding-002 inventory rows they close are named on each
// function.
//
// The organising constraint is that the engine's journal is WORKFLOW STATE,
// not a directory. The local runner reads its own history back with
// journal.OpenRead whenever it needs to look behind itself; the walk cannot,
// because reading a file is not replayable. So each ported behaviour is split
// the same way: the decision is a pure function over an event slice and an
// artifact resolver (exported from internal/runner or internal/gate, shared by
// both lanes), and this file supplies the engine's own view of those two
// things from the projection it is accumulating.

// projectedEvents renders the journal ops the walk has accumulated so far as
// the event log they will project to, together with a resolver for the
// artifact bytes those events reference.
//
// This is the engine's substitute for journal.OpenRead + Reader.Events, and it
// is exact rather than approximate, which is the whole point: writeProjectedRun
// replays each op as exactly ONE journal write (an artifact op becomes an
// artifact.recorded event, a span op a span.recorded, an append op the event
// itself), and journal's event log numbers events from 1 in write order. So op
// i projects to the event with Seq i+1. TestProjectedEventSeqMatchesProjection
// pins that correspondence against a really-projected run, because a learning
// episode's artifact NAME embeds the seq: if the two ever diverged the engine
// would name its episodes differently from the runner and the conformance
// diff would be the first thing to notice.
//
// Artifact bytes are inline in JournalArtifactOp.Data, so the resolver is a
// digest lookup with no I/O — and, unlike the runner's, it cannot fail for an
// artifact this run recorded.
func projectedEvents(proj JournalProjection) ([]journal.Event, func(journal.Ref) ([]byte, error), error) {
	events := make([]journal.Event, 0, len(proj.Ops))
	byName := map[string]journal.Ref{}
	byDigest := map[string][]byte{}
	byPath := map[string][]byte{}
	for i, op := range proj.Ops {
		seq := uint64(i + 1)
		switch op.Kind {
		case opArtifact:
			a := op.Artifact
			if a == nil {
				return nil, nil, fmt.Errorf("engine: journal op %d records no artifact", i)
			}
			ref, err := journal.ArtifactRef(a.Data)
			if err != nil {
				return nil, nil, fmt.Errorf("engine: address artifact %q: %w", a.Name, err)
			}
			byName[a.Name] = ref
			byDigest[ref.Digest] = a.Data
			byPath[ref.Path] = a.Data
			events = append(events, journal.Event{
				Seq: seq, Type: journal.EventArtifactRecorded,
				Stage: a.Stage, Attempt: a.Attempt, AttemptClass: a.Class,
				Name: a.Name, Ref: &ref, Integrity: a.Integrity,
			})
		case opSpan:
			s := op.Span
			if s == nil {
				return nil, nil, fmt.Errorf("engine: journal op %d records no span", i)
			}
			ref := s.Ref
			events = append(events, journal.Event{
				Seq: seq, Type: journal.EventSpanRecorded,
				Stage: s.Stage, Attempt: s.Attempt, AttemptClass: s.Class,
				Name: s.Name, Ref: &ref,
			})
		case opAppend:
			if op.Event == nil {
				return nil, nil, fmt.Errorf("engine: journal op %d appends no event", i)
			}
			ev := *op.Event
			ev.Seq = seq
			if ev.Type == journal.EventGateEvaluated && ev.Name != "" {
				// Exactly writeProjectedRun's wiring: the verdict artifact was
				// recorded by the op immediately before, and the event's Ref
				// is resolved from it.
				if ref, ok := byName[ev.Name]; ok {
					r := ref
					ev.Ref = &r
				}
			}
			events = append(events, ev)
		default:
			return nil, nil, fmt.Errorf("engine: journal op %d has unknown kind %q", i, op.Kind)
		}
	}
	resolve := func(ref journal.Ref) ([]byte, error) {
		if data, ok := byDigest[ref.Digest]; ok {
			return data, nil
		}
		if data, ok := byPath[ref.Path]; ok {
			return data, nil
		}
		return nil, fmt.Errorf("engine: artifact %q is not in this run's projection", ref.Path)
	}
	return events, resolve, nil
}

// projectedArtifactBytes adapts the same projection to internal/gate's
// ArtifactBytes seam, which resolves an apiv1.ArtifactPointer rather than a
// journal.Ref — the shape the finding-lifecycle helpers
// (gate.ReconcileLearningFindings, gate.DisproveReviewerFindings) consume.
func projectedArtifactBytes(resolve func(journal.Ref) ([]byte, error)) gate.ArtifactBytes {
	return func(ptr apiv1.ArtifactPointer) ([]byte, error) {
		return resolve(journal.Ref{Path: ptr.Path, Digest: ptr.Digest, Size: ptr.Size})
	}
}

// priorRepassCause is the walk's #3375 remediation-evidence input: when an
// agentic gate is about to evaluate an agentic subject that is being RE-ENTERED,
// why it is being re-entered. Inventory row: reviewer receives no repass cause.
//
// The classification is runner.PriorRepassCause, unchanged and unduplicated.
func priorRepassCause(rec *runJournal, subjectStage string) (*gate.RepassCause, error) {
	if rec == nil || subjectStage == "" {
		return nil, nil
	}
	events, resolve, err := projectedEvents(rec.proj)
	if err != nil {
		return nil, err
	}
	return runner.PriorRepassCause(events, subjectStage, resolve)
}

// cachedVerdictFor is the #3383 reviewer short-circuit: an implementation stage
// that already knows its own gate's answer — because it re-ran the very check
// the reviewer would run and serialized the verdict onto its outputs — hands it
// forward, and the gate routes on it WITHOUT dispatching the reviewer at all.
// Inventory row: cached verdict ignored (engine re-invokes the reviewer).
//
// Suppressed whenever the walk is carrying an instruction addendum, which is
// the runner's rule and the important one: an addendum means the previous
// answer was REJECTED, so honouring a cached verdict there would let a stage
// re-assert the very result the run just refused.
func cachedVerdictFor(subject apiv1.ResultEnvelope, instructionAddendum string) *apiv1.Verdict {
	if instructionAddendum != "" {
		return nil
	}
	return runner.CachedVerdictFromOutputs(subject.Outputs)
}

// stageArtifact commits one stage-attempt-scoped artifact the walk was told
// about by an activity (a salvage marker, a base-sync conflict detail, a
// captured unpushed diff) and hands back the pointer for it.
//
// The bytes come back on the activity RESULT rather than being written by the
// activity itself, which is the engine's rule for every artifact in this file:
// the projection commits workflow-derived bytes only, so an artifact whose
// bytes are not in history could not be re-derived by a replay or rebuilt by
// the repair projection.
func (r *runJournal) stageArtifact(
	ctx workflow.Context,
	stage string, attempt int, class journal.AttemptClass,
	name string, data []byte, mediaType string,
) (*apiv1.ArtifactPointer, error) {
	ref, err := journal.ArtifactRef(data)
	if err != nil {
		return nil, fmt.Errorf("engine: address artifact %q: %w", name, err)
	}
	r.artifactAt(workflow.Now(ctx), JournalArtifactOp{
		Stage: stage, Attempt: attempt, Class: class,
		Name: name, Data: data, Integrity: apiv1.IntegrityDerived,
	})
	return &apiv1.ArtifactPointer{
		Path: ref.Path, Digest: ref.Digest, Size: ref.Size,
		MediaType: mediaType, Integrity: apiv1.IntegrityDerived,
	}, nil
}

// recordSalvage commits the #724 salvage marker for an agentic attempt whose
// session timed out with committed work in the tree. Inventory row: onTimeout
// salvage drops its artifact.
//
// The engine's salvage DECISION is made in the activity (only the process
// holding the workspace can see whether the session left commits behind); this
// is the journal half, so the run records the same "we kept work a timeout
// would otherwise have thrown away" evidence the local runner records.
func (r *runJournal) recordSalvage(
	ctx workflow.Context, stage string, attempt int, class journal.AttemptClass, marker []byte,
) error {
	if len(marker) == 0 {
		return nil
	}
	_, err := r.stageArtifact(ctx, stage, attempt, class,
		runner.SalvageOnTimeoutArtifactName(stage), marker, "application/json")
	return err
}

// recordBaseSyncConflict commits the #813 conflict detail beside the business
// failure the walk is about to route. Inventory row: base-sync conflict lands
// without its detail artifact.
//
// The detail names the conflicting FILES, which is the only part of the
// failure a remediation stage can act on: the error message says the merge
// conflicted, the artifact says where.
func (r *runJournal) recordBaseSyncConflict(
	ctx workflow.Context, stage string, attempt int, class journal.AttemptClass, detail []byte,
) error {
	if len(detail) == 0 {
		return nil
	}
	_, err := r.stageArtifact(ctx, stage, attempt, class,
		runner.BaseSyncConflictArtifactName(stage), detail, "application/json")
	return err
}

// recordUnpushedDiff commits the #3366 capture: the patch a stage left
// UNPUSHED in its workspace, plus the sidecar naming the branch and base it
// was taken against. Inventory row: unpushed work is discarded with the
// workspace.
//
// It exists because a stage's workspace is torn down at the end of its
// attempt. Under the local runner that tear-down could strand a real diff the
// agent had committed but not pushed; under the engine it is worse, because
// the next attempt may land on a different worker entirely. The patch in the
// journal is what makes the work recoverable, and — through the continuity
// record — discoverable by the stage that resumes it.
func (r *runJournal) recordUnpushedDiff(
	ctx workflow.Context,
	in RunInput,
	stage string, attempt int, class journal.AttemptClass,
	capture *UnpushedDiffCapture,
) error {
	if capture == nil || len(capture.Diff) == 0 {
		return nil
	}
	patch, err := r.stageArtifact(ctx, stage, attempt, class,
		runner.UnpushedDiffPatchArtifactName(stage), capture.Diff, runner.ReviewerDiffMediaType)
	if err != nil {
		return err
	}
	meta := runner.UnpushedDiffMetadata{
		Schema:    runner.UnpushedDiffSchemaVersion,
		RunID:     in.RunID,
		Workflow:  in.WorkflowName,
		Stage:     stage,
		Attempt:   attempt,
		ItemIDs:   itemIDsFor(in.Item),
		ItemURL:   itemURLFor(in.Item),
		Branch:    capture.Branch,
		BaseRef:   capture.BaseRef,
		DiffBytes: len(capture.Diff),
		Diff:      *patch,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("engine: encode unpushed-diff metadata for stage %q: %w", stage, err)
	}
	_, err = r.stageArtifact(ctx, stage, attempt, class,
		runner.UnpushedDiffMetaArtifactName(stage), data, "application/json")
	return err
}

func itemIDsFor(item *apiv1.BacklogItem) []string {
	if item == nil || item.ID == "" {
		return nil
	}
	return []string{item.ID}
}

func itemURLFor(item *apiv1.BacklogItem) string {
	if item == nil {
		return ""
	}
	return item.URL
}

// recordReviewerDiff commits the #3384 reviewer diff: the patch the reviewer is
// actually being asked to judge, addressed as an artifact and handed to it as
// the "<gate>.diff" context pointer. Inventory row: reviewer evaluates without
// the subject diff.
//
// Its digest is load-bearing beyond the pointer. It is what makes the gate's
// two fast paths decidable — an EMPTY diff means the implementation stage
// changed nothing and there is nothing to review, and a diff IDENTICAL to the
// previous repass's means the stage was sent back and produced the same tree
// again — so the same bytes that inform the reviewer also decide whether the
// reviewer runs at all.
func (r *runJournal) recordReviewerDiff(
	ctx workflow.Context, runID, gateName string, diff []byte,
) (*apiv1.ContextPointer, error) {
	if len(diff) == 0 {
		return nil, nil
	}
	name := runner.ReviewerDiffArtifactName(runID, gateName)
	ref, err := journal.ArtifactRef(diff)
	if err != nil {
		return nil, fmt.Errorf("engine: address reviewer diff for gate %q: %w", gateName, err)
	}
	r.artifactAt(workflow.Now(ctx), JournalArtifactOp{
		Name: name, Data: diff, Integrity: apiv1.IntegrityDerived,
	})
	ptr := &apiv1.ArtifactPointer{
		Path: ref.Path, Digest: ref.Digest, Size: ref.Size,
		MediaType: runner.ReviewerDiffMediaType, Integrity: apiv1.IntegrityDerived,
	}
	return &apiv1.ContextPointer{
		Name: runner.ReviewerDiffPointerName(gateName), Integrity: apiv1.IntegrityDerived, Artifact: ptr,
	}, nil
}

// remediationEvidenceRequirement journals the #3375 requirement annotation: the
// gate is about to re-review a stage that was sent back, so the run records
// WHICH failure-evidence pointers the next attempt is obliged to have
// inspected before it may claim the work is blocked. Inventory row:
// remediation evidence is never demanded.
//
// Journaled BEFORE the evaluation rather than derived after it, exactly as the
// local runner does, so the obligation survives a crash: the receipt check on
// the far side reads this annotation back, and an obligation that existed only
// in memory would be silently dropped by a replay on another worker.
func (r *runJournal) remediationEvidenceRequirement(
	ctx workflow.Context, stage, gateName string, cause *gate.RepassCause, required []apiv1.ContextPointer,
) error {
	if cause == nil || len(required) == 0 {
		return nil
	}
	_, resolve, err := projectedEvents(r.proj)
	if err != nil {
		return err
	}
	r.append(ctx, journal.Event{
		Type: journal.EventRunnerAnnotation, Stage: stage, Gate: gateName,
		Runner: map[string]any{
			"kind":                            runner.RemediationEvidenceRequiredKind,
			"triggeringGate":                  cause.Gate,
			"triggeringStage":                 cause.Stage,
			"requiredFailureEvidencePointers": runner.RequiredContextPointerNames(required),
			"actionableEvidence": runner.RemediationEvidenceRequirements(
				required, projectedArtifactBytes(resolve),
			),
		},
	})
	return nil
}

// gateEvidence is what the walk establishes about an agentic gate's subject
// BEFORE it dispatches an evaluator (#3882): the repass cause, the
// remediation-evidence obligation the cause creates, and whether the subject
// already carried a verdict that makes an evaluator unnecessary.
//
// The diff-derived short-circuits are NOT here. They live inside ReviewGoober,
// for the same reason internal/gate's Evaluate puts them between its diff read
// and its Reviewer call: the diff is only observable from inside a provisioned
// workspace, and the reviewer already has one. Deciding them workflow-side
// would mean provisioning a second workspace per gate evaluation and adding an
// activity call ahead of every recorded ReviewGoober — which breaks replay of
// every history recorded before this change
// (TestContinuityPreChangeHistoryReplays).
type gateEvidence struct {
	// CachedVerdict is the subject's carried-forward verdict (#3383), nil when
	// there is none or when an instruction addendum suppresses it.
	CachedVerdict *apiv1.Verdict
	// CacheHit mirrors CachedVerdict != nil, journaled on gate.evaluated.
	CacheHit bool
	// RepassCause is why the subject stage was re-entered, nil on a first pass.
	RepassCause *gate.RepassCause
	// SubjectAgentic gates the empty-diff fast-fail: only an AGENTIC subject
	// that changed nothing is a fast-fail, since a deterministic one that
	// verifies or publishes legitimately produces no diff.
	SubjectAgentic bool
}

// collectGateEvidence gathers the above. A free function over the walk's state
// rather than a method: it is a pure decision over (definition, subject,
// projection) with no activity call at all.
func collectGateEvidence(
	ctx workflow.Context,
	m *wf.Machine,
	g apiv1.Gate,
	subjectStage string,
	subject apiv1.ResultEnvelope,
	pointers []apiv1.ContextPointer,
	instructionAddendum string,
	rec *runJournal,
) (gateEvidence, error) {
	var ev gateEvidence
	if g.Evaluator != apiv1.EvaluatorAgentic {
		return ev, nil
	}
	ev.CachedVerdict = cachedVerdictFor(subject, instructionAddendum)
	ev.CacheHit = ev.CachedVerdict != nil

	subjectTask, subjectIsTask := m.Task(subjectStage)
	ev.SubjectAgentic = subjectIsTask && subjectTask.Type == apiv1.TaskAgentic
	if !ev.SubjectAgentic || instructionAddendum != "" {
		return ev, nil
	}
	// #3375: why was the subject sent back? Resolved even when a cached
	// verdict will short-circuit the reviewer, because it is journaled on
	// gate.evaluated either way — the record of WHY a repass happened must not
	// depend on whether that repass needed a reviewer.
	cause, err := priorRepassCause(rec, subjectStage)
	if err != nil {
		return gateEvidence{}, fmt.Errorf("engine: resolve repass cause for gate %q: %w", g.Name, err)
	}
	ev.RepassCause = cause
	if cause == nil {
		return ev, nil
	}
	// The obligation is journaled BEFORE the evaluation, exactly as the local
	// runner journals it, so it survives a crash: an obligation held only in
	// memory would be silently dropped by a replay on another worker, and the
	// receipt check on the far side would then have nothing to check against.
	required := runner.RemediationFailureEvidencePointers(cause, pointers)
	if len(required) > 0 {
		if err := rec.remediationEvidenceRequirement(ctx, subjectStage, g.Name, cause, required); err != nil {
			return gateEvidence{}, err
		}
	}
	return ev, nil
}

// applyImplementationLaneOutcome writes the implementation lane's evidence
// onto a resolved gate result, mirroring the tail of internal/gate's
// resolveOutcome field for field.
//
// Three of these are decisions, not decoration, and that is why this is a
// function rather than four assignments at the call site:
//
//   - A DUPLICATE diff forces escalation. Without it, a stage that keeps
//     producing a byte-identical tree would ride the ordinary repass budget
//     down to exhaustion, one synthesized needs-changes at a time, which is
//     precisely the loop #316 exists to cut short. The runner escalates on the
//     first duplicate; so does this.
//   - An EMPTY diff forces escalation for the same reason and by the same
//     route (internal/gate passes emptyDiff as resolveOutcome's
//     forcedEscalation argument). The distinction matters more than it looks:
//     routing an empty-diff fail to the gate's own `fail` branch would send a
//     degenerate run down an ordinary failure path, when what has actually
//     happened is that an agentic stage reported success while doing nothing —
//     a condition no branch of the workflow can repair and a human has to see.
//   - repassCause is journaled ONLY for a duplicate diff. The runner is
//     deliberate about that: on any other evaluation the cause is inferable
//     from the events themselves, whereas an unchanged repass is the one case
//     where the annotation carries the only record of what the implementer was
//     asked to fix and did not.
//
// The reason ladder is the runner's, in the runner's order: a finding
// transition names itself, a duplicate diff overrides it (the finding
// reconcile is skipped for a synthesized verdict anyway, so they cannot both
// be set), and any other escalation — including the empty-diff one — falls
// back to the budget code, which is what the runner journals there too.
func applyImplementationLaneOutcome(g apiv1.Gate, gr *gateResult, ev gateEvidence, review GateReviewResult, findingReason string) {
	gr.DiffDigest = review.DiffDigest
	gr.DuplicateDiff = review.DuplicateDiff
	gr.CacheHit = ev.CacheHit
	reason := findingReason
	if review.DuplicateDiff {
		gr.RepassCause = ev.RepassCause
		reason = gate.ReasonUnchangedRepass
	}
	if (review.DuplicateDiff || review.EmptyDiff) && !gr.Escalated {
		gr.Escalated = true
		gr.Target = escalationTarget(g)
	}
	if gr.Escalated && reason == "" {
		reason = gate.ReasonRepassBudgetExhausted
		if gr.Outcome == gate.OutcomeInfra {
			reason = gate.ReasonInfrastructureBudgetExhausted
		}
	}
	gr.Reason = reason
}

// reconcileGateFindings applies the #3843 finding lifecycle to a reviewer's
// verdict: which of the PREVIOUS episodes' findings this one resolved or
// suppressed, which came back, and which the subject affirmatively disproved
// by showing the reviewer was wrong about the code.
//
// It runs BEFORE the outcome is resolved, and that ordering is the whole
// mechanism rather than an implementation detail. Both transitions can CHANGE
// the outcome — a needs-changes verdict whose every finding was already
// suppressed becomes a pass, and so does one whose every finding the diff
// disproves — so reconciling after resolveGateOutcome would journal a decision
// the findings no longer support and route the run on it.
//
// Delegated whole to internal/gate's shared helpers over a resolver backed by
// this run's own projection, so the two lanes cannot disagree about when a
// finding is settled. Prior episodes are read from the CONTEXT POINTERS, which
// is why the walk appends each injected learning episode to its pointer set:
// the pointer is not decoration, it is the input to this.
func reconcileGateFindings(
	g apiv1.Gate,
	verdict *apiv1.Verdict,
	pointers []apiv1.ContextPointer,
	diffDigest string,
	duplicateDiff, emptyDiff bool,
	rec *runJournal,
) (*apiv1.Verdict, string, findingLifecycle, error) {
	var lifecycle findingLifecycle
	if verdict == nil || g.Evaluator != apiv1.EvaluatorAgentic {
		return verdict, "", lifecycle, nil
	}
	// A synthesized verdict carries no findings, so it cannot prove anything
	// about the previous episode's — internal/gate's Evaluate skips the
	// reconcile for exactly these two, and so must this.
	if duplicateDiff || emptyDiff {
		return verdict, "", lifecycle, nil
	}
	_, resolve, err := projectedEvents(rec.proj)
	if err != nil {
		return nil, "", lifecycle, err
	}
	bytesFor := projectedArtifactBytes(resolve)

	reason := ""
	normalized, resolution := gate.ReconcileLearningFindings(*verdict, pointers, bytesFor, g.Name, diffDigest)
	verdict = &normalized
	lifecycle.Resolved = resolution.Resolved
	lifecycle.Suppressed = resolution.Suppressed
	lifecycle.Reopened = resolution.Reopened
	if resolution.AllSuppressed {
		reason = gate.ReasonFindingResolved
	}
	if normalized.Decision == apiv1.VerdictNeedsChanges {
		beforeFindings := append([]apiv1.Finding(nil), normalized.Findings...)
		before := gate.FindingIDs(beforeFindings)
		disproved, allDisproven := gate.DisproveReviewerFindings(normalized, pointers, bytesFor, g.Name)
		verdict = &disproved
		lifecycle.Disproven = gate.RemovedFindingIDs(before, disproved.Findings)
		lifecycle.DisprovenFindings = gate.RemovedFindings(beforeFindings, disproved.Findings)
		if allDisproven {
			reason = gate.ReasonFindingDisproven
		}
	}
	return verdict, reason, lifecycle, nil
}

// findingLifecycle is what one evaluation settled about the findings that came
// before it, journaled on gate.evaluated.
type findingLifecycle struct {
	Resolved          []string
	Suppressed        []string
	Reopened          []string
	Disproven         []string
	DisprovenFindings []apiv1.Finding
}

// contextNotInspectedRedispatch applies the #3374 ruling to a finished stage:
// a DEPENDENCY_NOT_MET the activity rejected as UNINSPECTED is re-dispatched
// once, carrying the rejection as its instruction addendum, instead of being
// allowed to escalate the run.
//
// Bounded at one because the second rejection means the agent inspected its
// inputs and still concluded it was blocked, and at that point the honest
// answer is the blocked one.
func contextNotInspectedRedispatch(t apiv1.Task, result apiv1.ResultEnvelope, rejected map[string]int) (bool, string) {
	if result.Status != apiv1.ResultBlocked || result.Error == nil ||
		result.Error.Code != runner.ContextNotInspectedCode {
		return false, ""
	}
	if rejected[t.Name] >= 1 {
		return false, ""
	}
	rejected[t.Name]++
	return true, runner.ContextNotInspectedAddendum(result.Error.Message)
}
