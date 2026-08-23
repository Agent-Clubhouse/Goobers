package runnercap

import (
	"regexp"
	"strings"
	"testing"
)

// labelValuePattern is the Kubernetes label-value grammar (empty excluded —
// a runner-class value is never empty).
var labelValuePattern = regexp.MustCompile(`^[a-z0-9A-Z]([a-z0-9A-Z._-]*[a-z0-9A-Z])?$`)

func TestRunnerClassValueEmptySet(t *testing.T) {
	if got := RunnerClassValue(nil); got != RunnerClassUnrestricted {
		t.Fatalf("RunnerClassValue(nil) = %q, want %q", got, RunnerClassUnrestricted)
	}
	if got := RunnerClassValue([]string{}); got != RunnerClassUnrestricted {
		t.Fatalf("RunnerClassValue([]) = %q, want %q", got, RunnerClassUnrestricted)
	}
}

func TestRunnerClassValueKnownSets(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"network:allowlist"}, "netallow"},
		{[]string{"network:none"}, "netnone"},
		{[]string{"fs:readonly-except-workspace"}, "fsro"},
		{[]string{"tmp:ephemeral"}, "tmpeph"},
		{[]string{"env:default-deny"}, "envdeny"},
		{
			[]string{"network:allowlist", "fs:readonly-except-workspace", "tmp:ephemeral"},
			"fsro-netallow-tmpeph",
		},
		{
			[]string{"env:default-deny", "fs:readonly-except-workspace", "network:allowlist", "network:none", "tmp:ephemeral"},
			"envdeny-fsro-netallow-netnone-tmpeph",
		},
	}
	for _, tc := range cases {
		if got := RunnerClassValue(tc.in); got != tc.want {
			t.Errorf("RunnerClassValue(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The value must be a pure function of the SET: permutations and duplicates
// cannot change it — a dispatcher stamping from one ordering and a renderer
// selecting from another must land on the same label.
func TestRunnerClassValueOrderAndDuplicateInvariant(t *testing.T) {
	a := RunnerClassValue([]string{"tmp:ephemeral", "network:allowlist", "fs:readonly-except-workspace"})
	b := RunnerClassValue([]string{"fs:readonly-except-workspace", "tmp:ephemeral", "network:allowlist", "tmp:ephemeral"})
	if a != b {
		t.Fatalf("permutation/duplicate changed the value: %q vs %q", a, b)
	}
}

// Every subset of the closed list must render to a valid Kubernetes label
// value, and distinct subsets must render to distinct values (the class IS the
// distinct restriction set — goobernetes-restrictions.md §12 render
// granularity).
func TestRunnerClassValueIsAlwaysAValidLabelValue(t *testing.T) {
	known := KnownRestrictions()
	values := make(map[string][]string)
	for mask := 0; mask < 1<<len(known); mask++ {
		var set []string
		for i, r := range known {
			if mask&(1<<i) != 0 {
				set = append(set, string(r))
			}
		}
		value := RunnerClassValue(set)
		if len(value) > 63 || !labelValuePattern.MatchString(value) {
			t.Errorf("RunnerClassValue(%v) = %q is not a valid label value", set, value)
		}
		if prior, dup := values[value]; dup {
			t.Errorf("distinct sets %v and %v collide on %q", prior, set, value)
		}
		values[value] = set
	}
}

// An effect outside the closed list must still produce a deterministic, valid
// value (the hash fallback), never a panic or an invalid label.
func TestRunnerClassValueUnknownEffectFallsBackToHash(t *testing.T) {
	value := RunnerClassValue([]string{"future:effect"})
	if !strings.HasPrefix(value, "rc-") {
		t.Fatalf("unknown effect should hash-fallback, got %q", value)
	}
	if len(value) > 63 || !labelValuePattern.MatchString(value) {
		t.Fatalf("fallback %q is not a valid label value", value)
	}
	if again := RunnerClassValue([]string{"future:effect"}); again != value {
		t.Fatalf("fallback not deterministic: %q vs %q", value, again)
	}
}

// The slug table and the closed restriction list must cover each other — a
// new effect added to one without the other would silently push every class
// containing it onto the opaque hash fallback.
func TestRunnerClassValueCoversClosedList(t *testing.T) {
	for _, r := range KnownRestrictions() {
		if _, ok := runnerClassSlugs[r]; !ok {
			t.Errorf("closed-list restriction %q has no runner-class slug", r)
		}
	}
	for r := range runnerClassSlugs {
		if !KnownRestriction(string(r)) {
			t.Errorf("runner-class slug exists for %q, which is not in the closed list", r)
		}
	}
}

// The annotation is a NON-DRIFTING mirror of the label: splitting the
// annotation back into a restriction set and re-deriving the value must return
// the same label value — for every permutation and duplicate, because both go
// through canonicalRestrictions. A future edit that lets them diverge fails
// HERE, not at 2am when an operator trusts a stale preimage.
func TestRunnerClassAnnotationRoundTrips(t *testing.T) {
	cases := [][]string{
		nil,
		{"fs:readonly-except-workspace"},
		{"network:allowlist", "fs:readonly-except-workspace", "tmp:ephemeral"},
		{"tmp:ephemeral", "network:allowlist", "fs:readonly-except-workspace"}, // permutation
		{"network:allowlist", "network:allowlist"},                             // duplicate
		{"env:default-deny", "network:none"},
		{"totally:unknown-effect"}, // rc-<sha> fallback still round-trips
	}
	for _, set := range cases {
		label := RunnerClassValue(set)
		ann := RunnerClassAnnotation(set)
		var decoded []string
		if ann != "" {
			decoded = strings.Split(ann, ",")
		}
		if got := RunnerClassValue(decoded); got != label {
			t.Errorf("round-trip: %v → annotation %q → RunnerClassValue %q, want %q", set, ann, got, label)
		}
	}
}
