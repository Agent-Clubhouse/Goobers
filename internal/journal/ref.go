package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// DigestAlgo is the digest algorithm every journal content address uses. sha256
// is the baseline; the algorithm is carried in the digest string ("sha256:…") so
// the format can grow without breaking readers.
const DigestAlgo = "sha256"

// Ref is a content-addressed pointer to a blob stored inside the run journal
// (an input snapshot or an artifact). Stages exchange Refs, never implicit
// shared state (ARCHITECTURE.md §5).
//
// Ref is the journal's on-disk production form of the stage contract's wire
// artifact pointer (`api/v1alpha1.ArtifactPointer`, owned by #10): same fields —
// journal-relative Path, sha256 Digest, Size, optional MediaType — so the runner
// maps journal→wire 1:1 with no field drift. The journal is the producer and
// content store; the envelope is the hand-off vehicle.
//
// Conformance (§3.3): Digest is normative — it is what makes runs comparable and
// what the conformance harness diffs. Path is runner-arranged storage detail and
// Size/MediaType are derived; none is compared across runners.
type Ref struct {
	// Path locates the blob relative to the run directory, e.g.
	// "artifacts/sha256/ab/cdef…". Informational; not conformance-normative.
	Path string `json:"path"`
	// Digest is the content address, "sha256:<hex>". Normative.
	Digest string `json:"digest"`
	// Size is the blob length in bytes. Derived; not normative.
	Size int64 `json:"size"`
	// MediaType optionally records the blob's media type (parity with the wire
	// ArtifactPointer). Empty when unknown; not conformance-normative.
	MediaType string `json:"mediaType,omitempty"`
	// Integrity is the blob's provenance grade. It is normative.
	Integrity apiv1.Integrity `json:"integrity,omitempty"`
}

// Digest computes the canonical "sha256:<hex>" content address of b. Callers
// scrub before digesting so the address commits to the scrubbed bytes.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return DigestAlgo + ":" + hex.EncodeToString(sum[:])
}

// ArtifactRef computes the Ref RecordArtifact would return for data without
// writing anything: Path, Digest, and Size are pure functions of the bytes.
// Callers that need an artifact's eventual journal address before a writer
// commits it — the engine workflow computing a verdict ContextPointer while
// the blob itself is written later by the history→journal projection (#629/
// #412) — derive it here so the content-address layout stays in one place.
// The digest commits to data exactly as given; RecordArtifact scrubs first,
// so the two agree only when data carries nothing registered for scrubbing.
func ArtifactRef(data []byte) (Ref, error) {
	digest := Digest(data)
	path, err := artifactPath(digest)
	if err != nil {
		return Ref{}, err
	}
	return Ref{Path: path, Digest: digest, Size: int64(len(data)), Integrity: apiv1.IntegrityDerived}, nil
}

// digestHex returns the hex portion of a "sha256:<hex>" digest.
func digestHex(digest string) (string, error) {
	algo, hexPart, ok := strings.Cut(digest, ":")
	if !ok || algo != DigestAlgo {
		return "", fmt.Errorf("journal: unsupported digest %q (want %q:<hex>)", digest, DigestAlgo)
	}
	if len(hexPart) != sha256.Size*2 {
		return "", fmt.Errorf("journal: malformed digest %q", digest)
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return "", fmt.Errorf("journal: malformed digest %q: %w", digest, err)
	}
	return hexPart, nil
}

// ArtifactPath is the journal-relative storage path an artifact with digest
// occupies inside a run directory — the exported form of the same fan-out
// layout ArtifactRef computes.
//
// It exists for readers that learn a digest without learning a path: the
// daemon's run-read routes address artifacts by digest alone (the read
// service's ArtifactMetadata deliberately omits journal-relative paths), so a
// client reconstructing a journal.Ref from that projection must derive Path
// from the one place the layout is defined rather than restating the shard
// rule a second time.
func ArtifactPath(digest string) (string, error) { return artifactPath(digest) }

// artifactPath is the fan-out storage path for an artifact blob, sharding by the
// first byte of the hex digest to keep directories shallow.
func artifactPath(digest string) (string, error) {
	hexPart, err := digestHex(digest)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s/%s/%s", dirArtifacts, DigestAlgo, hexPart[:2], hexPart[2:]), nil
}

// spanPath is the fan-out storage path for a span blob, mirroring artifactPath
// but rooted under spans/.
func spanPath(digest string) (string, error) {
	hexPart, err := digestHex(digest)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s/%s/%s", dirSpans, DigestAlgo, hexPart[:2], hexPart[2:]), nil
}
