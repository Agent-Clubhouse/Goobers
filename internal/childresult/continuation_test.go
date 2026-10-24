package childresult

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
)

// The continuation pointer is a contained, digest-named path a pod can
// materialize, and is recognized only by its name and path together.
func TestContinuationPointerIsContainedAndRecognized(t *testing.T) {
	digest := "sha256:ab0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"
	if !blobstore.ValidDigest(digest) {
		t.Fatalf("fixture digest %q invalid", digest)
	}
	p := ContinuationPointer(digest, 42)
	if p.Artifact == nil || p.Artifact.Validate() != nil || p.Artifact.Digest != digest || p.Artifact.Size != 42 || p.RunID != "" {
		t.Fatalf("pointer = %+v", p)
	}
	if !IsContinuationPointer(p) {
		t.Fatal("the continuation pointer is not recognized")
	}
	for name, other := range map[string]apiv1.ContextPointer{
		"another name": {Name: "implement.artifact[0]", Artifact: p.Artifact},
		"another path": {Name: ContinuationPointerName, Artifact: &apiv1.ArtifactPointer{Path: "artifacts/x.json", Digest: digest}},
		"no artifact":  {Name: ContinuationPointerName},
	} {
		if IsContinuationPointer(other) {
			t.Fatalf("%s: recognized as a continuation", name)
		}
	}
}
