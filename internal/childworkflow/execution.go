package childworkflow

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/goobers/goobers/internal/blobstore"
)

// DecodeStartEnvelope refuses unknown fields and unsupported versions. A child
// receipt must never be interpreted as an ordinary named-catalog request.
func DecodeStartEnvelope(payload []byte) (ChildStartEnvelope, error) {
	var e ChildStartEnvelope
	d := json.NewDecoder(bytes.NewReader(payload))
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		return e, ErrSubmissionInvalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return e, ErrSubmissionInvalid
	}
	if e.Kind != ChildStartKind || e.Version != 1 || !submissionText(e.Gaggle, 128) || !submissionText(e.ParentRunID, 256) || !submissionText(e.ParentStage, 256) || !submissionText(e.StageOccurrence, 256) || !submissionText(e.InvocationKey, 256) || !submissionText(e.Workflow, 256) {
		return e, ErrSubmissionInvalid
	}
	if e.MaxChildren < 1 || e.MaxChildren > 32 || (e.Backend != BackendRunner && e.Backend != BackendEngine) {
		return e, ErrSubmissionInvalid
	}
	for _, pin := range []string{e.ConfigGeneration, e.ParentWorkflowDigest, e.ParentGooberDigest, e.SourceDigest, e.CanonicalDigest, e.ConfigDigest, e.PolicyDigest, e.WorkflowDigest, e.PlacementsDigest} {
		if !blobstore.ValidDigest(pin) {
			return e, ErrSubmissionInvalid
		}
	}
	return e, nil
}

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
