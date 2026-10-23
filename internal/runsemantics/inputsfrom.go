package runsemantics

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// stageOutput is one completed stage's journaled Outputs together with the
// provenance of the content that produced them. Outputs are bare scalars with
// nowhere to carry a label of their own, so a consumer resolving inputsFrom
// grades the producing stage (TBH-4).
type stageOutput struct {
	Outputs   map[string]any  `json:"-"`
	Integrity apiv1.Integrity `json:"-"`
}

// stageOutputs is every completed stage's journaled Outputs, keyed by stage
// name. The runner already records these; #562 just makes them addressable.
type stageOutputs map[string]stageOutput

func (s stageOutputs) Record(stage string, outputs map[string]any, grade apiv1.Integrity) {
	if stage == "" {
		return
	}
	if s == nil {
		return
	}
	copied := make(map[string]any, len(outputs))
	for k, v := range outputs {
		copied[k] = v
	}
	s[stage] = stageOutput{Outputs: copied, Integrity: grade}
}

// put copies an already-recorded stage entry, preserving its provenance. Used
// when merging one stageOutputs into another (branch fan-in, clones).
func (s stageOutputs) Put(stage string, produced stageOutput) {
	if stage == "" || s == nil {
		return
	}
	s.Record(stage, produced.Outputs, produced.Integrity)
}

// integrityOf returns the provenance recorded for a completed stage. An unknown
// stage returns the zero grade, which fails admission closed.
func (s stageOutputs) IntegrityOf(stage string) apiv1.Integrity {
	return s[stage].Integrity
}

func (s stageOutputs) Clear(stage string) {
	if s != nil {
		delete(s, stage)
	}
}

// resolveInputsFrom resolves one inputsFrom value against the run's completed
// stage outputs.
//
// The resolution rule is deliberately conservative, and it is the rule a prior
// attempt at #562 parked on getting wrong:
//
//	Treat the value as a STAGE-QUALIFIED reference only when the segment
//	before the first dot names a stage that has actually produced outputs in
//	this run. Otherwise treat the ENTIRE string as a bare output key.
//
// That ordering is what preserves legacy dotted keys — an output literally
// named "a.b" keeps working, because "a" is not a stage. Stage names containing
// a dot are rejected at compile time, so there is no ambiguity to escape and no
// escaping syntax to invent.
//
// Bare keys keep their exact current meaning: they resolve against the
// immediately preceding stage only. That is what makes this fully
// backward-compatible.
func resolveInputsFrom(value string, upstream apiv1.ResultEnvelope, completed stageOutputs, qualified bool) (any, bool) {
	if stage, key, ok := splitQualified(value); ok && qualified {
		if produced, seen := completed[stage]; seen {
			v, found := produced.Outputs[key]
			return v, found
		}
		// The prefix is not a stage that ran — fall through and treat the whole
		// value as a bare key, which is what keeps a legacy dotted key working.
	}
	v, ok := upstream.Outputs[value]
	return v, ok
}

// inputsFromIntegrity returns the provenance of whatever resolveInputsFrom would
// bind for value. It deliberately mirrors that function's branch order rather
// than re-deriving it: the grade must describe the value actually bound, so if
// the two ever disagree the admission check would be guarding the wrong stage.
func inputsFromIntegrity(value string, upstream apiv1.ResultEnvelope, completed stageOutputs, qualified bool) apiv1.Integrity {
	if stage, _, ok := splitQualified(value); ok && qualified {
		if _, seen := completed[stage]; seen {
			return completed.IntegrityOf(stage)
		}
	}
	return upstream.Integrity
}

// splitQualified splits "<stage>.<key>" on the FIRST dot. An output key may
// itself contain dots; a stage name may not.
func splitQualified(value string) (stage, key string, ok bool) {
	stage, key, found := strings.Cut(value, ".")
	if !found || stage == "" || key == "" {
		return "", "", false
	}
	return stage, key, true
}

// inputsFromError builds the stage-closed failure for an unresolvable
// reference. InputsFrom is a contract, not a hint, so a miss fails the stage
// rather than silently omitting the input.
//
// Every arm names the KEYS that were available and never their values (#5247):
// an author needs to know what they could have bound, while a diagnostic that
// printed the values would leak an entire upstream result into a failure
// message that lands in journals, comments and logs.
//
// The middle arm exists because of resolveInputsFrom's deliberate fallthrough.
// A dotted value whose prefix is not a stage that ran is treated as a WHOLE
// bare key — that is what keeps a legacy dotted output key such as "a.b"
// working — but it means a typo'd or not-yet-run stage prefix silently
// degrades into a bare lookup and then misses. Reporting "upstream output
// \"plan.prTitle\" not found" for that case describes the lookup that was
// performed while hiding the rule that chose it, which is the single most
// confusing way this can fail.
func inputsFromError(
	taskName, inputKey, value string,
	upstream apiv1.ResultEnvelope,
	completed stageOutputs,
	qualified bool,
) error {
	err := inputsFromMissError(taskName, inputKey, value, upstream, completed, qualified)
	if cause := upstreamFailureCause(upstream); cause != "" {
		return fmt.Errorf("%w; the preceding stage had FAILED (%s), which is why it produced no such output", err, cause)
	}
	return err
}

// upstreamFailureCause names why the preceding stage failed, or "" when it did
// not. A failed stage whose Next is a gate still advances (the gate branches on
// the honest failed status), so a consumer past that gate can find its input
// missing because of the failure, not because the stage forgot to emit it
// (#6890). Without this the run's only diagnostic names a missing output and
// the provider error that caused it is buried in an earlier stage.
func upstreamFailureCause(upstream apiv1.ResultEnvelope) string {
	if upstream.Status != apiv1.ResultFailure {
		return ""
	}
	if upstream.Error == nil || upstream.Error.Message == "" {
		return "no error detail"
	}
	message := upstream.Error.Message
	if len(message) > 300 {
		message = message[:300] + "…"
	}
	if upstream.Error.Code != "" {
		return upstream.Error.Code + ": " + message
	}
	return message
}

func inputsFromMissError(
	taskName, inputKey, value string,
	upstream apiv1.ResultEnvelope,
	completed stageOutputs,
	qualified bool,
) error {
	if stage, key, ok := splitQualified(value); ok && qualified {
		if produced, seen := completed[stage]; seen {
			return fmt.Errorf("task %q: inputsFrom %q: stage %q produced no output %q (it emitted: %s)",
				taskName, inputKey, stage, key, joinKeys(produced.Outputs))
		}
		return fmt.Errorf(
			"task %q: inputsFrom %q: no stage %q has produced outputs in this run, so %q was treated as a "+
				"single output key of the preceding stage, which emitted: %s",
			taskName, inputKey, stage, value, joinKeys(upstream.Outputs))
	}
	return fmt.Errorf("task %q: inputsFrom %q: upstream output %q not found (the preceding stage emitted: %s)",
		taskName, inputKey, value, joinKeys(upstream.Outputs))
}

func joinKeys(outputs map[string]any) string {
	if len(outputs) == 0 {
		return "nothing"
	}
	keys := make([]string, 0, len(outputs))
	for k := range outputs {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return strings.Join(keys, ", ")
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// StageResultIntegrity is the grade a stage's ResultEnvelope finishes with: the
// weaker of the grade its executor stamped on the envelope itself and the grade
// producedIntegrity derived from what the stage was admitted with (#2979).
//
// An executor that knows its own output is less trustworthy than its inputs —
// external-telemetry returns third-party data, so it stamps IntegrityUnapproved —
// must not have that grade replaced by the input-derived one, or a deterministic
// stage with no graded input would hand its unapproved outputs downstream as
// trusted. Taking the weaker of the two, rather than letting the executor's grade
// win outright, means an executor stamp can only ever LOWER a grade: an
// executor that read unapproved input cannot launder it upward by stamping a
// stronger grade (TBH-4). An executor that stamps nothing keeps producedIntegrity
// exactly as before, and an unknown stamp yields the zero grade, which
// downstream admission refuses (fail closed).
//
// Exported for the Temporal engine, which must grade a stage on the same rule.
func StageResultIntegrity(executorGrade, produced apiv1.Integrity) apiv1.Integrity {
	if executorGrade == "" {
		return produced
	}
	return apiv1.WeakestIntegrity(executorGrade, produced)
}

// StageOutputs is every completed stage's Outputs keyed by stage name, the
// state stage-qualified resolution reads. It is an alias rather than a defined
// type so the runner's own internals keep using the unexported spelling.
type StageOutputs = stageOutputs

// NewStageOutputs builds an empty completed-stage map.
func NewStageOutputs() StageOutputs { return stageOutputs{} }

// RecordCompleted applies the record-or-forget rule a finished task owes the
// map: a tolerated failure (ContinueOnError) is FORGOTTEN, anything else is
// recorded under the stage's name with the provenance grade of the content that
// produced it. Recording copies the outputs — a walk keeps handing the same
// ResultEnvelope onward, and a shared map would let a later mutation rewrite an
// earlier stage's recorded outputs.
//
// Forgetting is what a tolerated failure must do: downstream stages must not
// consume partial results, and a qualified reference to the forgotten stage
// falls through to bare-key resolution.
//
// Shared because "which results become addressable" is the same question as
// "what does <stage>.<key> resolve to", and answering it differently on the two
// runners is the divergence this whole export exists to prevent.
func (s stageOutputs) RecordCompleted(t apiv1.Task, result apiv1.ResultEnvelope) {
	if result.Status == apiv1.ResultFailure && t.ContinueOnError {
		s.Clear(t.Name)
		return
	}
	s.Record(t.Name, result.Outputs, result.Integrity)
}

// ResolveInputsFrom exports resolveInputsFrom for the engine walk.
func ResolveInputsFrom(value string, upstream apiv1.ResultEnvelope, completed StageOutputs, qualified bool) (any, bool) {
	return resolveInputsFrom(value, upstream, completed, qualified)
}

// InputsFromIntegrity exports inputsFromIntegrity — the provenance of whatever
// ResolveInputsFrom would bind for the same arguments.
func InputsFromIntegrity(value string, upstream apiv1.ResultEnvelope, completed StageOutputs, qualified bool) apiv1.Integrity {
	return inputsFromIntegrity(value, upstream, completed, qualified)
}

// InputsFromError exports inputsFromError, so an unresolvable reference reads
// identically to an operator whichever runner walked the definition.
func InputsFromError(
	taskName, inputKey, value string,
	upstream apiv1.ResultEnvelope,
	completed StageOutputs,
	qualified bool,
) error {
	return inputsFromError(taskName, inputKey, value, upstream, completed, qualified)
}
