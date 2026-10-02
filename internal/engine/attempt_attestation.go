package engine

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/launchreceipt"
)

// StageAttemptAttestationArtifact adapts trusted assembly to the existing
// byte-preserving artifact projection. This inert PR3A seam does not add a
// workflow command: PR3B must activate all dispatch paths under GetVersion.
// IntegrityDerived describes artifact provenance, not verified execution or
// receipt authority. Readers must revalidate through launchreceipt.Reader.
func StageAttemptAttestationArtifact(p launchreceipt.Projection) (JournalArtifactOp, error) {
	binding, ref, data := p.Binding(), p.Reference(), p.Bytes()
	if ref.Name == "" || len(data) == 0 || len(data) > launchreceipt.MaxBytes {
		return JournalArtifactOp{}, launchreceipt.ErrInvalid
	}
	return JournalArtifactOp{Branch: binding.Branch, Stage: binding.Stage, Attempt: binding.Number,
		Class: binding.Class, Name: ref.Name, Data: data, Integrity: apiv1.IntegrityDerived}, nil
}
