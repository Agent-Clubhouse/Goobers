package runsemantics

import (
	"bytes"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// ReviewerDiffRedactionKind identifies the evidence linking a reviewer patch to
// the committed source bytes before the credential scrubber transformed it.
const ReviewerDiffRedactionKind = "reviewer-diff-redaction"

// ReviewerDiffRedaction is recorded by both runner and engine. It carries only
// digests and sizes, never the unscrubbed patch.
type ReviewerDiffRedaction struct {
	SourceDigest string `json:"sourceDigest"`
	SourceBytes  int    `json:"sourceBytes"`
	Bytes        int    `json:"bytes"`
	Redacted     bool   `json:"redacted"`
}

// ObserveReviewerDiffRedaction captures the local runner's pre-artifact scrub
// evidence. The recorder's final content digest is supplied when journaling.
func ObserveReviewerDiffRedaction(source, scrubbed []byte) ReviewerDiffRedaction {
	return ReviewerDiffRedaction{SourceDigest: journal.Digest(source), SourceBytes: len(source), Bytes: len(scrubbed), Redacted: !bytes.Equal(source, scrubbed)}
}

// Event renders the shared annotation without changing the local wire shape.
func (r ReviewerDiffRedaction) Event(gate string, ref journal.Ref) journal.Event {
	return journal.Event{Type: journal.EventRunnerAnnotation, Gate: gate, Runner: map[string]any{
		"kind": ReviewerDiffRedactionKind, "redacted": r.Redacted, "sourceDigest": r.SourceDigest,
		"sourceBytes": r.SourceBytes, "digest": ref.Digest, "bytes": r.Bytes,
	}}
}

// PreparedReviewerDiffPointer validates the runner-owned preparation against
// the same-run context pointer that the worker or stage pod will materialize.
// Nil preparation is the N-1 path; observed empty preparation has no pointer.
func PreparedReviewerDiffPointer(env apiv1.InvocationEnvelope) (*apiv1.ContextPointer, error) {
	prepared := env.PreparedReviewerDiff
	if prepared == nil || prepared.Artifact == nil {
		return nil, nil
	}
	ref := prepared.Artifact
	path, err := journal.ArtifactPath(ref.Digest)
	if err != nil || !prepared.Observed || ref.Path != path || ref.Size <= 0 || ref.Integrity != apiv1.IntegrityDerived || ref.MediaType != ReviewerDiffMediaType {
		return nil, fmt.Errorf("prepared reviewer diff: invalid recorded artifact")
	}
	name := ReviewerDiffPointerName(strings.TrimPrefix(env.TaskID, env.RunID+":"))
	var found *apiv1.ContextPointer
	for _, pointer := range env.ContextPointers {
		if pointer.Name != name {
			continue
		}
		if found != nil || pointer.Artifact == nil || *pointer.Artifact != *ref || pointer.RunID != "" || pointer.Integrity != ref.Integrity {
			return nil, fmt.Errorf("prepared reviewer diff: context pointer %q does not match recorded evidence", name)
		}
		copy := pointer
		found = &copy
	}
	if found == nil {
		return nil, fmt.Errorf("prepared reviewer diff: context pointer %q is missing", name)
	}
	return found, nil
}
