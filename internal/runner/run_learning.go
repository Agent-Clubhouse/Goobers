package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/learning"
)

// gateBranchInjection is what a gate branch that re-enters a stage produces
// for the walk that took it: the reviewer's verdict pointer, and the learning
// episode's pointer when the branch is one the shared predicate admits.
//
// It exists for the CONCURRENT branch walker, which has no walkState to hand
// injectLearningEpisode and so cannot use that method's routing. Both walkers
// therefore share the production (recordGateBranchEpisode) while each routes
// into its own accumulator: the sequential walk into ws.pointers or the active
// branch, runBranch into result.pointers with its artifact/produced accounting.
type gateBranchInjection struct {
	verdict *apiv1.ContextPointer
	episode *apiv1.ContextPointer
}

// pointers returns the pointers to accumulate, verdict first — the order the
// sequential walk has always appended them in, and therefore the order a
// re-entered stage's envelope carries them in.
func (i gateBranchInjection) pointers() []apiv1.ContextPointer {
	out := make([]apiv1.ContextPointer, 0, 2)
	if i.verdict != nil {
		out = append(out, *i.verdict)
	}
	if i.episode != nil {
		out = append(out, *i.episode)
	}
	return out
}

// recordGateBranchInjection is the CONCURRENT branch walker's whole gate-branch
// arm, produced once so it cannot drift from the sequential walk's (#3932).
//
// The local runner has two walks that evaluate a gate and take its branch:
// stepGate/walk, which serve the sequential walk and every branch of a
// sequentially-executed parallel, and runBranch, which serves the concurrent
// walk when maxConcurrentBranches > 1. runBranch carried a hand-copied HALF of
// the arm — the verdict pointer and not the learning episode — so
// maxConcurrentBranches, a scheduling bound tuned for machine capacity and
// routinely different between a laptop, CI and a deployment, decided whether a
// repass received its correction, its context pointer and its derived-integrity
// downgrade. Nothing in the DSL says that bound is semantic, and it is not.
//
// The two arms are kept identical by construction rather than by review: this
// helper produces both halves, and the episode half is the shared
// recordGateBranchEpisode the sequential walk reaches through
// injectLearningEpisode. A walker cannot acquire a different predicate or a
// different episode without changing the one place both read.
//
// replayed is the resume guard runBranch already applied to the verdict
// pointer: a branch resuming across a gate.evaluated boundary re-derives the
// gate result from history rather than evaluating it, and its previously
// recorded pointers are rebuilt by pendingParallel. Recording the artifact
// again would double-count it and file a second annotation for one injection.
// The sequential walk has no such boundary.
func recordGateBranchInjection(
	jr executionJournal,
	in StartInput,
	gateName, target string,
	gr gate.Result,
	sourceStage string,
	sourceResult apiv1.ResultEnvelope,
	replayed bool,
) (gateBranchInjection, error) {
	var out gateBranchInjection
	if replayed {
		return out, nil
	}
	if gr.VerdictArtifact != nil {
		out.verdict = &apiv1.ContextPointer{
			Name: gateName + ".verdict", Integrity: gr.VerdictArtifact.Integrity, Artifact: gr.VerdictArtifact,
		}
	}
	episode, err := recordGateBranchEpisode(jr, in, gateName, target, gr, sourceStage, sourceResult, out.verdict)
	if err != nil {
		return gateBranchInjection{}, err
	}
	out.episode = episode
	return out, nil
}

// recordGateBranchEpisode is THE learning-injection producer: one predicate and
// one construction, reached by every arm that can re-enter a stage.
//
// #3929 ruled which branches owe an episode and #3943 spelled the ruling as
// LearningEpisodeAppliesToBranch, shared with the engine. #3932's point is that
// the ruling has to be APPLIED in one place too: the runner reaches a
// stage-re-entering branch from four arms — stepGate's retry arm, walk()'s
// advance path, and runBranch's own two — and a predicate re-read per arm is a
// predicate that will eventually differ per arm. It already had.
//
// Returns a nil pointer, and journals nothing, for a branch the predicate
// declines. That is a disposition (a forward branch, a terminal, an escalation)
// rather than a correction: a stage that has not run has produced nothing to
// correct.
func recordGateBranchEpisode(
	jr executionJournal,
	in StartInput,
	gateName, target string,
	gr gate.Result,
	sourceStage string,
	sourceResult apiv1.ResultEnvelope,
	verdictPointer *apiv1.ContextPointer,
) (*apiv1.ContextPointer, error) {
	if !LearningEpisodeAppliesToBranch(LearningEpisodeBranchFor(gr)) {
		return nil, nil
	}
	return recordLearningInjection(jr, in, gateName, target, gr, sourceStage, sourceResult, verdictPointer)
}

// injectLearningEpisode commits the repass correction and hands the re-entered
// stage a pointer to it, scoping that pointer to the active parallel branch
// when one is running.
//
// It is a method with two call sites — stepGate's retry arm and walk()'s
// advance path — because the branches that owe an episode are split across
// them by a condition that has nothing to do with the episode: whether
// retryFailureClassForGateResult happens to classify the failure. Both pass
// the same already-appended "<gate>.verdict" pointer as the episode's
// evidence, so the artifact bytes do not depend on which arm the branch
// travelled.
//
// Neither call site reads the LearningEpisodeAppliesToBranch predicate itself
// (#3932): it lives inside recordGateBranchEpisode, with the construction, so
// that the concurrent branch walker — which has no walkState and so cannot use
// this method at all — cannot acquire a different answer to the same question.
// This method is the ROUTING half; the production is shared.
//
// The bool reports that the run has TERMINATED (the caller must return the
// accompanying Result and error): a correction that cannot be journaled must
// not silently route the run.
func (r *Runner) injectLearningEpisode(
	ctx context.Context, ws *walkState, g apiv1.Gate, target string,
	gr gate.Result, verdictPointer *apiv1.ContextPointer,
) (Result, bool, error) {
	episode, err := recordGateBranchEpisode(
		ws.jr, ws.in, g.Name, target, gr, ws.lastStage, ws.lastResult, verdictPointer,
	)
	if err != nil {
		terminal, failErr := r.failTerminal(ctx, ws.in.RunID, ws.jr, ws.in.RepoRef, g.Name, ws.steps,
			fmt.Errorf("runner: journal learning episode injection for gate %q: %w", g.Name, err))
		return terminal, true, failErr
	}
	if episode != nil {
		if ws.parallel != nil {
			ws.parallel.recordCurrentPointer(*episode)
		} else {
			ws.pointers = append(ws.pointers, *episode)
		}
	}
	return Result{}, false, nil
}

func recordLearningInjection(
	jr executionJournal,
	in StartInput,
	gateName, target string,
	result gate.Result,
	sourceStage string,
	sourceResult apiv1.ResultEnvelope,
	pointer *apiv1.ContextPointer,
) (*apiv1.ContextPointer, error) {
	if jr == nil {
		return nil, nil
	}
	addressing := learningEpisodeAddressing(jr.Dir(), gateName, sourceStage, target, result.Verdict != nil)
	sourceSeq, sourceAttempt := addressing.SourceSeq, addressing.SourceAttempt
	if sourceAttempt == 0 {
		sourceAttempt = result.Attempt
	}
	// The episode's BYTES are built by the shared builder (parity3882.go), not
	// here: the artifact's digest is conformance-normative, so the engine's own
	// injection has to produce the identical struct rather than a second
	// hand-assembled copy of it.
	episode := BuildLearningEpisode(LearningEpisodeInput{
		RunID:             in.RunID,
		Workflow:          in.Machine.Def.Name,
		WorkflowDigest:    in.Machine.Digest(),
		GooberDigest:      in.GooberDigest,
		Gate:              gateName,
		Stage:             sourceStage,
		SourceSeq:         sourceSeq,
		SourceAttempt:     sourceAttempt,
		TargetNextAttempt: addressing.TargetNextAttempt,
		Verdict:           result.Verdict,
		SourceResult:      sourceResult,
		VerdictPointer:    pointer,
	})
	data, err := json.Marshal(episode)
	if err != nil {
		return nil, fmt.Errorf("encode learning episode: %w", err)
	}
	name := LearningEpisodeArtifactName(gateName, sourceSeq)
	ref, err := jr.RecordArtifact(name, data)
	if err != nil {
		return nil, fmt.Errorf("record learning episode: %w", err)
	}
	episodePointer := &apiv1.ContextPointer{
		Name:      LearningEpisodePointerName(sourceSeq),
		Integrity: ref.Integrity,
		Artifact: &apiv1.ArtifactPointer{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size,
			MediaType: "application/json", Integrity: ref.Integrity,
		},
	}
	runner := LearningEpisodeAnnotation(episode, target, ref.Path, ref.Digest)
	if err := jr.Append(journal.Event{
		Type: journal.EventRunnerAnnotation,
		// #3931: Stage is the TARGET and so is Attempt. A stage-scoped event's
		// Attempt is that stage's attempt number, and the injection is
		// evidence for the invocation it FEEDS — which, on a nontrivial
		// send-back, is not the subject's attempt plus one.
		Stage:     target,
		Attempt:   episode.NextAttempt,
		Name:      name,
		Ref:       &ref,
		Integrity: ref.Integrity,
		Runner:    runner,
	}); err != nil {
		return nil, err
	}
	return episodePointer, nil
}

func learningEpisodeAddressing(runDir, gateName, sourceStage, target string, reviewer bool) LearningEpisodeAddressing {
	rd, err := journal.OpenRead(runDir)
	if err != nil {
		return LearningEpisodeAddressing{}
	}
	events, err := rd.Events()
	if err != nil {
		return LearningEpisodeAddressing{}
	}
	return ResolveLearningEpisodeAddressing(events, gateName, sourceStage, target, reviewer)
}

func learningFindingsForRepass(gateName, stage string, verdict *apiv1.Verdict, result apiv1.ResultEnvelope) []apiv1.Finding {
	if verdict != nil && len(verdict.Findings) > 0 {
		findings := append([]apiv1.Finding(nil), verdict.Findings...)
		for i := range findings {
			learning.NormalizeFinding(&findings[i], gateName, findings[i].EvidenceDigest)
		}
		return findings
	}
	message := strings.TrimSpace(result.Summary)
	if result.Error != nil {
		if result.Error.Message != "" {
			message = result.Error.Message
		}
		if message == "" {
			message = result.Error.Code
		}
	}
	if message == "" {
		message = "validation failed"
	}
	finding := apiv1.Finding{
		Severity:               apiv1.SeverityError,
		Message:                message,
		Location:               stage,
		LearningClassification: apiv1.LearningValidation,
	}
	learning.NormalizeFinding(&finding, stage, learningEvidenceDigest(result.Artifacts))
	return []apiv1.Finding{finding}
}

func learningEvidence(pointer *apiv1.ContextPointer, verdict *apiv1.Verdict, result apiv1.ResultEnvelope) []apiv1.ArtifactPointer {
	var evidence []apiv1.ArtifactPointer
	if pointer != nil && pointer.Artifact != nil {
		evidence = append(evidence, *pointer.Artifact)
	}
	if verdict != nil {
		evidence = append(evidence, verdict.Evidence...)
	}
	evidence = append(evidence, result.Artifacts...)
	seen := map[string]bool{}
	out := evidence[:0]
	for _, artifact := range evidence {
		key := artifact.Digest + "\x00" + artifact.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, artifact)
	}
	return out
}

func learningEvidenceDigest(artifacts []apiv1.ArtifactPointer) string {
	digests := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		if artifact.Digest != "" {
			digests = append(digests, artifact.Digest)
		}
	}
	slices.Sort(digests)
	if len(digests) == 0 {
		return ""
	}
	return apiv1.Digest([]byte(strings.Join(digests, "\n")))
}
