package v1alpha1

import (
	"fmt"
	"sort"

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

// ValidateResolvedInputIntegrity enforces minimum over inputsFrom values that
// have already been resolved to a producing stage.
//
// Outputs are bare scalars and carry no provenance of their own (see
// ResultEnvelope.Outputs), so each entry here is keyed by the consuming input
// name and graded by the ResultEnvelope.Integrity of the stage that produced it.
// A stage with no recorded grade fails closed, the same as an unlabeled context
// pointer: an unlabeled input is exactly the case an attacker would arrange.
//
// This is the second half of ValidateInputIntegrity. Both must run before a
// stage is provisioned, because provisioning is what hands the stage a workspace
// and credentials (TBH-4).
func ValidateResolvedInputIntegrity(inputs map[string]Integrity, minimum Integrity) error {
	if minimum == "" {
		return nil
	}
	if !minimum.Valid() {
		return &IntegrityAdmissionError{Input: "minimumIntegrity", Minimum: minimum, Reason: "unknown minimum integrity"}
	}
	// Sorted so a stage with several failing inputs always names the same one.
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := admitIntegrity(name, inputs[name], minimum); err != nil {
			return err
		}
	}
	return nil
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
//
// contextFrom is a filter over upstream PRODUCERS, so it can only speak about
// pointers whose names carry one — the source-scoped classes. A
// SYSTEM-GENERATED pointer has no producing workflow state and no source name
// a workflow author could ever write to select it, so it is outside the
// filter's jurisdiction and survives selection (#3928).
//
// The concrete case is the injected learning episode. A repassing gate mints
// "learning.episode[<seq>]" and addresses it to the one stage it is sending
// back; dropping it here discarded the correction feedback before the stage
// was dispatched, and before ValidateInputIntegrity could grade it, on every
// stage that declares contextFrom — which is every stage on the flagship
// implementation lane. Selection is still a filter, not an opening: an
// unclassified name, including a MALFORMED one that resembles a system
// pointer, matches nothing and is dropped, and source-scoped pointers keep
// their source scoping exactly as before.
//
// Order is preserved: callers thread the result straight into an invocation
// envelope whose digest is on the conformance surface.
func SelectContextPointers(pointers []ContextPointer, sources []string) []ContextPointer {
	if len(sources) == 0 {
		return pointers
	}
	selected := make([]ContextPointer, 0, len(pointers))
	for _, pointer := range pointers {
		class, source := ClassifyContextPointer(pointer.Name)
		if class.SystemGenerated() {
			selected = append(selected, pointer)
			continue
		}
		if !class.SourceScoped() {
			continue
		}
		for _, want := range sources {
			if source == want {
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
