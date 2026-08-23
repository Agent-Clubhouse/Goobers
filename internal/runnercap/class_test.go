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

// The preimage annotation exists so an operator can decode an opaque class
// value (issue #3568): for EVERY subset of the closed list — and for sets the
// hash fallback covers — decoding the preimage and re-deriving the value must
// land on the selector value, because both are functions of the same
// CanonicalRestrictions output.
func TestRunnerClassPreimageRoundTripsToValue(t *testing.T) {
	known := KnownRestrictions()
	for mask := 0; mask < 1<<len(known); mask++ {
		var set []string
		for i, r := range known {
			if mask&(1<<i) != 0 {
				set = append(set, string(r))
			}
		}
		preimage := RunnerClassPreimage(set)
		if got, want := RunnerClassValue(ParseRunnerClassPreimage(preimage)), RunnerClassValue(set); got != want {
			t.Errorf("preimage %q round-trips to %q, want %q", preimage, got, want)
		}
	}

	// The hash-fallback case is the one the annotation exists for: the value
	// is opaque, so the preimage is the only human-readable handle.
	future := []string{"future:effect", "network:none"}
	preimage := RunnerClassPreimage(future)
	if preimage != "future:effect,network:none" {
		t.Fatalf("preimage = %q, want canonical sorted comma join", preimage)
	}
	if got, want := RunnerClassValue(ParseRunnerClassPreimage(preimage)), RunnerClassValue(future); got != want {
		t.Fatalf("fallback preimage %q round-trips to %q, want %q", preimage, got, want)
	}
}

func TestRunnerClassPreimageEmptySet(t *testing.T) {
	if got := RunnerClassPreimage(nil); got != "" {
		t.Fatalf("RunnerClassPreimage(nil) = %q, want empty", got)
	}
	if got := ParseRunnerClassPreimage(""); len(got) != 0 {
		t.Fatalf("ParseRunnerClassPreimage(\"\") = %v, want empty set", got)
	}
	if got := RunnerClassValue(ParseRunnerClassPreimage("")); got != RunnerClassUnrestricted {
		t.Fatalf("empty preimage round-trips to %q, want %q", got, RunnerClassUnrestricted)
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
