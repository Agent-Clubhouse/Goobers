package childworkflow

// ValidateRetainedStart recompiles exact retained source using trusted pinned
// definitions intersected with current permission/placement authority. The
// caller obtains a from a runtime resolver, never from the receipt or request.
// Accepted custody survives attempt replacement; this is an execution-policy
// check, not reuse of an expired tool grant. Every pinned input must still match.
func ValidateRetainedStart(a Authority, e ChildStartEnvelope, source []byte) (*Proposal, error) {
	if a.Admission.Gaggle.Name != e.Gaggle || a.Origin.Gaggle != e.Gaggle || a.Origin.RunID != e.ParentRunID || a.Origin.StageOccurrence != e.StageOccurrence {
		return nil, ErrAuthorityUnavailable
	}
	v, err := NewValidator(a.Admission)
	if err != nil {
		return nil, err
	}
	p, err := v.Validate(source)
	if err != nil {
		return nil, err
	}
	expected, err := childStartEnvelope(a, p, e.InvocationKey)
	if err != nil {
		return nil, err
	}
	if e != expected {
		return nil, ErrSubmissionInvalid
	}
	return p, nil
}
