package runner

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// reviewerContextPointers is the local runner's binding of
// ReviewerContextPointers: it reads the run journal and withholds superseded
// validation evidence from an agentic reviewer of an agentic subject. A
// journal that cannot be read leaves the pointers unchanged; the gate's own
// repass-cause resolution reads the same journal and fails the gate closed.
func reviewerContextPointers(jr executionJournal, machine *workflow.Machine, subjectStage string, pointers []apiv1.ContextPointer) []apiv1.ContextPointer {
	if jr == nil || machine == nil || len(pointers) == 0 {
		return pointers
	}
	if subject, ok := machine.Task(subjectStage); !ok || subject.Type != apiv1.TaskAgentic {
		return pointers
	}
	reader, err := journal.OpenRead(jr.Dir())
	if err != nil {
		return pointers
	}
	events, err := reader.Events()
	if err != nil {
		return pointers
	}
	return ReviewerContextPointers(events, subjectStage, deterministicTaskPredicate(machine), pointers)
}

func deterministicTaskPredicate(machine *workflow.Machine) func(string) bool {
	return func(stage string) bool {
		task, ok := machine.Task(stage)
		return ok && task.Type == apiv1.TaskDeterministic
	}
}

// ReviewerContextPointers removes validation evidence that the subject stage
// has since superseded from the context handed to an agentic reviewer (#5901).
//
// A deterministic stage whose result a gate judged non-pass (local-ci failing
// local-gate, say) after it tested the subject stage's output produced its
// artifacts against the revision that existed then. Once the subject stage has
// completed again after that verdict, the branch the reviewer is about to
// judge is a later revision, and that old evidence no longer describes it.
// Showing it anyway lets a reviewer cite a defect the remediation already
// fixed, request the completed fix again, and drive the run into an
// unchanged-repass escalation. The gate's pass branch re-runs the validation
// against the reviewed revision, so withholding the old evidence never skips
// it. Evidence that never tested the subject's output is kept: a dedicated
// remediation stage (a CI failure routed to remediate-ci) is dispatched to
// fix exactly that evidence, and its reviewer needs it.
//
// Only reviewer context is filtered. The unfiltered pointers remain the
// remediation evidence the subject stage itself was obliged to inspect.
// isDeterministic limits removal to validation-stage artifacts; agentic
// output, gate verdicts, and learning episodes are always kept.
//
// Exported, and pure over an event slice, so the durable engine applies the
// same rule to its journal projection rather than a lookalike.
func ReviewerContextPointers(
	events []journal.Event,
	subjectStage string,
	isDeterministic func(stage string) bool,
	pointers []apiv1.ContextPointer,
) []apiv1.ContextPointer {
	superseded := supersededValidationArtifacts(events, subjectStage, isDeterministic)
	if len(superseded) == 0 {
		return pointers
	}
	kept := make([]apiv1.ContextPointer, 0, len(pointers))
	for _, pointer := range pointers {
		if !isSupersededPointer(pointer, superseded) {
			kept = append(kept, pointer)
		}
	}
	return kept
}

type supersededArtifact struct {
	stage string
	path  string
}

func supersededValidationArtifacts(events []journal.Event, subjectStage string, isDeterministic func(string) bool) map[supersededArtifact]struct{} {
	if subjectStage == "" || isDeterministic == nil {
		return nil
	}
	subjectFinished := -1
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventStageFinished && events[i].Stage == subjectStage && !isInterruptedAttemptMarker(events[i]) {
			subjectFinished = i
			break
		}
	}
	superseded := map[supersededArtifact]struct{}{}
	lastFinished := map[int]int{}
	priorSubject := -1
	for i := 0; i < subjectFinished; i++ {
		event := events[i]
		switch event.Type {
		case journal.EventStageFinished:
			if isInterruptedAttemptMarker(event) {
				continue
			}
			lastFinished[event.Branch] = i
			if event.Stage == subjectStage {
				priorSubject = i
			}
		case journal.EventGateEvaluated:
			if event.Verdict == gate.OutcomePass {
				continue
			}
			judged, ok := lastFinished[event.Branch]
			// Evidence that never tested the subject's own output (the CI
			// failure a dedicated remediation stage was dispatched to fix)
			// is the subject's brief, not a stale verdict on its work.
			if !ok || priorSubject < 0 || priorSubject > judged {
				continue
			}
			stage := events[judged].Stage
			if stage == subjectStage || !isDeterministic(stage) {
				continue
			}
			for _, ref := range events[judged].Artifacts {
				superseded[supersededArtifact{stage: stage, path: ref.Path}] = struct{}{}
			}
		}
	}
	return superseded
}

func isSupersededPointer(pointer apiv1.ContextPointer, superseded map[supersededArtifact]struct{}) bool {
	if pointer.Artifact == nil || pointer.RunID != "" {
		return false
	}
	stage, _, ok := strings.Cut(pointer.Name, ".artifact[")
	if !ok {
		return false
	}
	_, found := superseded[supersededArtifact{stage: stage, path: pointer.Artifact.Path}]
	return found
}
