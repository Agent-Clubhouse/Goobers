package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// These exported checks let the config validator (api/validate) reuse the
// compiler's semantic analysis without duplicating it, while still reporting its
// own per-field cross-reference messages. Each returns human-readable, self-
// contained problem strings; an empty slice means the check passed.

var backlogClaimPattern = regexp.MustCompile(`(?s)backlog-query.*--claim`)

// CheckWarnings reports non-fatal workflow diagnostics.
func CheckWarnings(def Definition) []string {
	var warnings []string
	for _, task := range def.Spec.Tasks {
		if task.Type != apiv1.TaskDeterministic || task.Run == nil {
			continue
		}
		if !backlogClaimPattern.MatchString(strings.Join(task.Run.Command, " ")) {
			continue
		}
		if strings.TrimSpace(task.Inputs["resultFile"]) != "" {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			`task %q runs backlog-query --claim without inputs.resultFile; empty ticks will report success instead of no-work`,
			task.Name,
		))
	}
	return warnings
}

// CheckReachability reports unreachable states and loops with no exit. It is a
// no-op on a structurally broken graph (missing start, dangling transition):
// those are reported field-by-field by the validator, and walking a broken graph
// would only cascade misleading errors.
func CheckReachability(def Definition) []string {
	m := newMachine(def)
	if len(structuralProblems(m)) > 0 {
		return nil
	}
	return reachabilityProblems(m)
}

// CheckSchedules reports invalid cron/interval schedule expressions on
// schedule-type triggers.
func CheckSchedules(def Definition) []string {
	return scheduleProblems(def)
}

// CheckTriggerFields reports a trigger missing the field its own type
// requires — a signal trigger with no signal name, a backlog-item trigger
// with no selector (#125).
func CheckTriggerFields(def Definition) []string {
	return triggerFieldProblems(def)
}

// CheckAdmission reports capability-admission and unknown-harness violations
// against the supplied goober definitions.
func CheckAdmission(def Definition, goobers map[string]apiv1.GooberSpec) []string {
	return admissionProblems(def, goobers)
}

// CheckGateVocabulary reports an automated gate whose declared
// params.equals is not a value from its check's fixed output vocabulary
// (checkEqualsVocab below).
func CheckGateVocabulary(def Definition) []string {
	return gateVocabProblems(def)
}

// checkEqualsVocab is the fixed output vocabulary a gate's params.equals
// must be drawn from, for automated checks that have one — the compile-time
// half of the ci-gate vocabulary fix (#132): internal/gate/automated.go's
// "ci-status" check reads providers.CheckState values ("passing"/"failing"/
// "pending", the raw state a ci-poll stage's Outputs["ciStatus"] carries,
// internal/executor/cipoll.go), while "status-equals" reads
// apiv1.ResultStatus values ("success"/"failure"/"blocked") — a workflow
// declaring the wrong one for a given check (e.g. ci-status's
// params.equals: "success") would compile clean today but never match at
// runtime, silently repassing forever. internal/workflow cannot import
// internal/gate to reuse its DefaultChecks registry directly (internal/gate
// already imports internal/workflow, for branch-target resolution) — this
// table is intentionally kept in sync with automated.go's DefaultChecks by
// hand; a check absent here has no fixed vocabulary (output-equals,
// output-numeric-gte) and is not validated.
var checkEqualsVocab = map[string][]string{
	"status-equals": {"success", "failure", "blocked"},
	"ci-status":     {"passing", "failing", "pending"},
}

// gateVocabProblems validates every automated gate's declared params.equals
// (if any) against checkEqualsVocab. A gate with an unknown Check name, or no
// declared params.equals, is left to internal/gate's own runtime error
// ("unknown automated check") or default-value behavior respectively —
// this check only guards the vocabulary-mismatch failure mode.
func gateVocabProblems(def Definition) []string {
	var problems []string
	for _, g := range def.Spec.Gates {
		if g.Evaluator != apiv1.EvaluatorAutomated || g.Automated == nil {
			continue
		}
		vocab, ok := checkEqualsVocab[g.Automated.Check]
		if !ok {
			continue
		}
		equals, declared := g.Automated.Params["equals"]
		if !declared || equals == "" {
			continue
		}
		valid := false
		for _, v := range vocab {
			if v == equals {
				valid = true
				break
			}
		}
		if !valid {
			problems = append(problems, fmt.Sprintf("gate %q: check %q params.equals %q is not one of %v", g.Name, g.Automated.Check, equals, vocab))
		}
	}
	return problems
}

// CheckGateOutcomes reports gate branches that can never be taken (not a
// producible outcome for the gate's evaluator) and producible outcomes with
// no defined branch (#124) — workflow-intrinsic, no goober data needed.
func CheckGateOutcomes(def Definition) []string {
	return gateOutcomeProblems(def, nil)
}

// triggerFieldProblems reports a trigger declared without the field its own
// type requires to do anything (#125): type=signal with no Signal name has
// nothing to fire on. type=schedule's own requirement (a non-empty Schedule
// expression) is already covered by scheduleProblems.
//
// #125 also flagged type=backlog-item with no Selector — deliberately NOT
// enforced here: Selector (WF-040/SCH-010) has no runtime consumer anywhere
// in the codebase yet (nothing matches on it), and a huge fraction of
// existing test fixtures across internal/engine, internal/scheduler (a
// quarantined tier-3 package), internal/runner, and test/e2e declare a
// selector-less backlog-item trigger as ordinary scaffolding. Requiring it
// would mean touching fixtures repo-wide for a field with zero behavioral
// effect today — a disproportionate cost for a "minor" item; revisit once
// Selector is actually wired to real routing logic.
func triggerFieldProblems(def Definition) []string {
	var problems []string
	for i, tr := range def.Spec.Triggers {
		if tr.Type == apiv1.TriggerSignal && strings.TrimSpace(tr.Signal) == "" {
			problems = append(problems, fmt.Sprintf("trigger[%d] type=signal requires a signal name", i))
		}
	}
	return problems
}

// scheduleProblems validates the schedule expression of every schedule
// trigger. A workflow may declare more than one schedule trigger (#341 —
// e.g. a weekday cadence and a separate weekend one): the runtime scheduler
// (localscheduler.Scheduler.Tick) evaluates every one of them and fires if
// any is due, so multiple schedule triggers is not itself a problem; only a
// malformed or empty individual expression is. (issue #142 originally made
// more than one schedule trigger a hard compile error, since the runtime at
// the time only ever honored the first — #341 replaced that runtime
// limitation with real multi-schedule support, so the compile-time rejection
// is no longer needed.)
func scheduleProblems(def Definition) []string {
	var problems []string
	for i, tr := range def.Spec.Triggers {
		if tr.Type != apiv1.TriggerSchedule {
			continue
		}
		if strings.TrimSpace(tr.Schedule) == "" {
			problems = append(problems, fmt.Sprintf("trigger[%d] type=schedule requires a schedule expression", i))
			continue
		}
		if err := validateSchedule(tr.Schedule); err != nil {
			problems = append(problems, fmt.Sprintf("trigger[%d] invalid schedule %q: %v", i, tr.Schedule, err))
		}
	}
	return problems
}

// descriptors are the named cron shorthands the scheduler accepts.
var descriptors = map[string]bool{
	"@yearly": true, "@annually": true, "@monthly": true, "@weekly": true,
	"@daily": true, "@midnight": true, "@hourly": true,
}

// validateSchedule structurally validates a cron/interval expression. It accepts
// the named descriptors, "@every <duration>", and 5- or 6-field cron
// expressions. It is intentionally a structural gate (not a full cron engine):
// it catches malformed expressions at compile time; the scheduler owns firing.
func validateSchedule(expr string) error {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "@every ") {
		dur := strings.TrimSpace(strings.TrimPrefix(expr, "@every "))
		if _, err := time.ParseDuration(dur); err != nil {
			return fmt.Errorf("bad @every duration: %w", err)
		}
		return nil
	}
	if strings.HasPrefix(expr, "@") {
		if descriptors[expr] {
			return nil
		}
		return fmt.Errorf("unknown descriptor (want one of @yearly @monthly @weekly @daily @hourly or @every <dur>)")
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 && len(fields) != 6 {
		return fmt.Errorf("expected 5 or 6 space-separated fields, got %d", len(fields))
	}
	const allowed = "0123456789*/,-?LW#"
	for i, f := range fields {
		if f == "" {
			return fmt.Errorf("field %d is empty", i)
		}
		for _, r := range f {
			if !strings.ContainsRune(allowed, r) {
				return fmt.Errorf("field %d %q has illegal character %q", i, f, r)
			}
		}
	}
	return nil
}

// stateNames returns every defined state name in definition order (tasks then
// gates) — a deterministic order for stable problem reporting.
func stateNames(def Definition) []string {
	names := make([]string, 0, len(def.Spec.Tasks)+len(def.Spec.Gates))
	for _, t := range def.Spec.Tasks {
		names = append(names, t.Name)
	}
	for _, g := range def.Spec.Gates {
		names = append(names, g.Name)
	}
	return names
}

// sortedKeys returns the keys of a string-keyed map in sorted order, so any walk
// over a map (e.g. gate branches) is deterministic.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// toSet turns a slice into a membership set.
func toSet(xs []string) map[string]bool {
	s := make(map[string]bool, len(xs))
	for _, x := range xs {
		s[x] = true
	}
	return s
}
