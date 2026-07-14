package workflow

import (
	"fmt"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// These exported checks let the config validator (api/validate) reuse the
// compiler's semantic analysis without duplicating it, while still reporting its
// own per-field cross-reference messages. Each returns human-readable, self-
// contained problem strings; an empty slice means the check passed.

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

// scheduleProblems validates the schedule expression of every schedule trigger.
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
