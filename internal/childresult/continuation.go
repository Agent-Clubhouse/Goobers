package childresult

import (
	"errors"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// The continuation an engine parent resumes with is a pointer to its child's
// sealed result receipt (#8085 d), materialized into the resumed stage by
// digest from the blob plane.
const (
	ContinuationPointerName = "child-result"
	continuationPathPrefix  = "child-results/"
)

// ErrResultRetired reports a continuation whose sealed result is no longer
// retained: the child was retired after its retention window, and blob GC
// may collect its receipt. The parent's stage cannot read its child's result.
var ErrResultRetired = errors.New("child result retired after retention: the child's sealed result is no longer retained (#8085)")

// ContinuationPointer points a resumed parent at a sealed result receipt.
func ContinuationPointer(receiptDigest string, size int64) apiv1.ContextPointer {
	return apiv1.ContextPointer{Name: ContinuationPointerName, Artifact: &apiv1.ArtifactPointer{
		Path: continuationPathPrefix + strings.TrimPrefix(receiptDigest, "sha256:") + ".json", Digest: receiptDigest, Size: size,
	}}
}

// IsContinuationPointer reports whether p is a child-result continuation.
func IsContinuationPointer(p apiv1.ContextPointer) bool {
	return p.Name == ContinuationPointerName && p.Artifact != nil && strings.HasPrefix(p.Artifact.Path, continuationPathPrefix)
}
