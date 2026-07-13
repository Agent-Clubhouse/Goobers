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
