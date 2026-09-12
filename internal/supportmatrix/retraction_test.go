package supportmatrix

import (
	"strings"
	"testing"
)

// TestEffectiveInWithoutRetractionIsRefused is the #4708 regression: removing
// the declared retraction restores today's refusal. The real DSL 1.4 entry's
// effectiveIn (v0.4.0) predates its published transition (v0.5.0), so without
// a matching Retraction it must be refused, not silently accepted.
func TestEffectiveInWithoutRetractionIsRefused(t *testing.T) {
	matrix := GetDSL()
	row := matrix[V1DSLVersion]
	row.Retraction = nil
	matrix[V1DSLVersion] = row

	err := ValidateSupportPolicy(matrix)
	if err == nil || !strings.Contains(err.Error(), "requires a declared retraction") {
		t.Fatalf("ValidateSupportPolicy = %v, want refusal for the undeclared retraction", err)
	}
}

// TestRetractionMustMatchPublishedCommitment is #4708's AC: a retraction for
// a version that was never published as unsupportedAfter is itself refused.
// It cannot be used to excuse an early drop that was never actually promised.
func TestRetractionMustMatchPublishedCommitment(t *testing.T) {
	matrix := GetDSL()
	row := matrix[V1DSLVersion]
	row.Retraction = &Retraction{
		UnsupportedAfter: "v0.9.0", // never the published date (v0.5.0)
		Release:          "v0.4.0",
		Rationale:        "not the real promise",
	}
	matrix[V1DSLVersion] = row

	err := ValidateSupportPolicy(matrix)
	if err == nil || !strings.Contains(err.Error(), `but "v0.5.0" was published`) {
		t.Fatalf("ValidateSupportPolicy = %v, want refusal for a retraction naming an unpublished commitment", err)
	}
}

// TestRetractionReleaseMustMatchEffectiveIn: a retraction's Release must name
// the same release effectiveIn does — they describe the same real-world
// event (when the corrected level actually took effect).
func TestRetractionReleaseMustMatchEffectiveIn(t *testing.T) {
	matrix := GetDSL()
	row := matrix[V1DSLVersion]
	row.Retraction = &Retraction{
		UnsupportedAfter: "v0.5.0",
		Release:          "v0.3.9", // does not match effectiveIn (v0.4.0)
		Rationale:        "mismatched release",
	}
	matrix[V1DSLVersion] = row

	err := ValidateSupportPolicy(matrix)
	if err == nil || !strings.Contains(err.Error(), "does not match effectiveIn") {
		t.Fatalf("ValidateSupportPolicy = %v, want refusal for a retraction release mismatched with effectiveIn", err)
	}
}

// TestRetractionRequiresRationale: an empty rationale is refused — the
// audit trail is the point of a declared retraction, not merely its
// existence.
func TestRetractionRequiresRationale(t *testing.T) {
	matrix := GetDSL()
	row := matrix[V1DSLVersion]
	row.Retraction = &Retraction{
		UnsupportedAfter: "v0.5.0",
		Release:          "v0.4.0",
		Rationale:        "   ",
	}
	matrix[V1DSLVersion] = row

	err := ValidateSupportPolicy(matrix)
	if err == nil || !strings.Contains(err.Error(), "must declare a rationale") {
		t.Fatalf("ValidateSupportPolicy = %v, want refusal for a retraction with no rationale", err)
	}
}

// TestRetractionRequiresValidReleaseFormat: a malformed release is refused
// the same way any other release string in this package is.
func TestRetractionRequiresValidReleaseFormat(t *testing.T) {
	matrix := GetDSL()
	row := matrix[V1DSLVersion]
	row.EffectiveIn = "v0.4.0"
	row.Retraction = &Retraction{
		UnsupportedAfter: "v0.5.0",
		Release:          "v0.4.0", // must equal EffectiveIn to reach the format check
		Rationale:        "invalid release format",
	}
	// Corrupt Release AFTER matching effectiveIn so the mismatch check
	// doesn't mask the format check this test targets.
	row.EffectiveIn = "not-a-version"
	matrix[V1DSLVersion] = row

	err := ValidateSupportPolicy(matrix)
	if err == nil {
		t.Fatal("ValidateSupportPolicy = nil, want refusal for an invalid effectiveIn version")
	}
}

// TestPublishedRetractionCannotChange mirrors
// TestPublishedEffectiveInCannotBeRemoved: once a retraction is part of a
// released matrix, evolution refuses any change to it — the audit record
// must not be editable after publication.
func TestPublishedRetractionCannotChange(t *testing.T) {
	released, current := GetDSL(), GetDSL()
	row := current[V1DSLVersion]
	row.Retraction = &Retraction{
		UnsupportedAfter: row.Retraction.UnsupportedAfter,
		Release:          row.Retraction.Release,
		Rationale:        "a different rationale than what was published",
	}
	current[V1DSLVersion] = row

	err := validateSupportMatrixEvolution(released, current, "v0.4.0",
		map[string]releaseVersion{"1.4": {minor: 1}, "2.0": {minor: 1}})
	if err == nil || !strings.Contains(err.Error(), "retraction must not change") {
		t.Fatalf("evolution = %v, want immutable retraction", err)
	}
}

// TestEffectiveInAtOrAfterItsTransitionIsRefused: effectiveIn only makes
// sense as a correction for a level that took effect BEFORE its published
// date. A value at or after that date corrects nothing and is refused
// outright, regardless of whether a retraction is declared.
func TestEffectiveInAtOrAfterItsTransitionIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name        string
		effectiveIn string
	}{
		{"equal to the published transition", "v0.5.0"},
		{"after the published transition", "v0.6.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matrix := GetDSL()
			row := matrix[V1DSLVersion]
			row.EffectiveIn = tc.effectiveIn
			// Keep the real (mismatched) retraction: this must be refused by
			// the "nothing to correct" check before retraction is even
			// considered.
			matrix[V1DSLVersion] = row

			err := ValidateSupportPolicy(matrix)
			if err == nil || !strings.Contains(err.Error(), "must be earlier than the published transition") {
				t.Fatalf("ValidateSupportPolicy = %v, want refusal for a no-op effectiveIn", err)
			}
		})
	}
}
