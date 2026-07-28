package v1alpha1

import (
	"fmt"
	"strings"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// Integrity is the canonical provenance grade carried across stage contracts.
type Integrity = apiintegrity.Grade

// Integrity grade aliases keep stage contracts aligned with the shared API.
const (
	IntegrityTrusted    = apiintegrity.Trusted
	IntegrityMaintainer = apiintegrity.Maintainer
	IntegrityUnapproved = apiintegrity.Unapproved
	IntegrityDerived    = apiintegrity.Derived
)

// WeakestIntegrity returns the least trustworthy valid grade, or the zero grade
// when no valid aggregate can be formed.
func WeakestIntegrity(grades ...Integrity) Integrity {
	return apiintegrity.Weakest(grades...)
}

// IntegrityAdmissionErrorCode is journaled when a stage refuses input below its
// declared minimum integrity.
const IntegrityAdmissionErrorCode = "input_integrity_below_minimum"

// IntegrityAdmissionError identifies the input that failed stage admission.
// +kubebuilder:object:generate=false
type IntegrityAdmissionError struct {
	Input   string
	Actual  Integrity
	Minimum Integrity
	Reason  string
}

func (e *IntegrityAdmissionError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("input %q integrity admission failed: %s (actual %q, minimum %q)", e.Input, e.Reason, e.Actual, e.Minimum)
	}
	return fmt.Sprintf("input %q has integrity %q below minimum %q", e.Input, e.Actual, e.Minimum)
}

// ValidateInputIntegrity enforces minimum over the content-bearing inputs in an
// invocation. Missing or contradictory labels fail closed once a minimum is set.
func ValidateInputIntegrity(item *BacklogItem, pointers []ContextPointer, minimum Integrity) error {
	if minimum == "" {
		return nil
	}
	if !minimum.Valid() {
		return &IntegrityAdmissionError{Input: "minimumIntegrity", Minimum: minimum, Reason: "unknown minimum integrity"}
	}
	if item != nil {
		if err := admitIntegrity("item", item.Integrity, minimum); err != nil {
			return err
		}
	}
	for i := range pointers {
		pointer := pointers[i]
		actual := pointer.Integrity
		if pointer.Artifact != nil {
			artifactIntegrity := pointer.Artifact.Integrity
			if actual != "" && artifactIntegrity != "" && actual != artifactIntegrity {
				return &IntegrityAdmissionError{
					Input: pointer.Name, Actual: actual, Minimum: minimum,
					Reason: fmt.Sprintf("context pointer label contradicts artifact label %q", artifactIntegrity),
				}
			}
			if actual == "" {
				actual = artifactIntegrity
			}
		}
		if err := admitIntegrity(pointer.Name, actual, minimum); err != nil {
			return err
		}
	}
	return nil
}

// SelectContextPointers returns only pointers produced by the named workflow
// states. An empty source list preserves the accumulated context unchanged.
func SelectContextPointers(pointers []ContextPointer, sources []string) []ContextPointer {
	if len(sources) == 0 {
		return pointers
	}
	selected := make([]ContextPointer, 0, len(pointers))
	for _, pointer := range pointers {
		for _, source := range sources {
			if pointer.Name == source+".verdict" ||
				strings.HasPrefix(pointer.Name, source+".artifact[") {
				selected = append(selected, pointer)
				break
			}
		}
	}
	return selected
}

func admitIntegrity(input string, actual, minimum Integrity) error {
	if !actual.Valid() {
		return &IntegrityAdmissionError{Input: input, Actual: actual, Minimum: minimum, Reason: "input has no valid integrity label"}
	}
	if !actual.Meets(minimum) {
		return &IntegrityAdmissionError{Input: input, Actual: actual, Minimum: minimum}
	}
	return nil
}
