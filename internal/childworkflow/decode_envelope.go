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
	if e.Kind != ChildStartKind || e.Version != 1 || !submissionText(e.Gaggle, 128) || !submissionText(e.ParentRunID, 256) || !submissionText(e.ParentStage, 256) || !submissionText(e.StageOccurrence, 256) || !submissionText(e.InvocationKey, 256) || !submissionText(e.Workflow, 256) || !submissionText(e.ParentWorkflow, 256) {
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
