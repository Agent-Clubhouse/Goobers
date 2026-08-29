package workerhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
)

// MaterializeContext makes a stage's ContextPointers readable on THIS node.
//
// It is the fetch half of the distributed data plane; StagingArtifacts.record
// is the write-through half. Together they replace an assumption — that the
// process running a stage can open the file its predecessor wrote — with a
// mechanism.
//
// The harness resolves each pointer as <stagingRoot>/<ref.Path> on the local
// filesystem, so this runs first and puts the bytes there. For a pointer whose
// blob this worker produced, the file already exists and nothing happens; for
// one produced on another node, its digest is fetched from the fleet store and
// written where Resolve will look. The local directory is a cache, and this is
// the cache fill.
//
// FAILING SOFT IS DELIBERATE. A digest the store does not have is left absent
// rather than raised here, so the executor's own fail-closed integrity check is
// still the thing that refuses the stage, with its own diagnostic and its own
// classification. A fetch layer that invented a second, earlier failure mode for
// the same condition would make an integrity fault look like an infrastructure
// one, which is exactly the distinction #622 exists to keep. A store that is
// broken rather than merely incomplete does surface — that is not the same
// condition and must not be silent.
//
// With no store configured this is a no-op, which is the tier-1 case: one
// runner, one directory, every blob local by construction.
//
// A POINTER IS UNTRUSTED INPUT and this function WRITES FILES, so every path is
// validated before it is joined — see the refusal below.
func MaterializeContext(ctx context.Context, store blobstore.Store, stagingRoot string, pointers []apiv1.ContextPointer) error {
	if store == nil || stagingRoot == "" || len(pointers) == 0 {
		return nil
	}
	for i := range pointers {
		ref := pointers[i].Artifact
		if ref == nil || ref.Path == "" || ref.Digest == "" {
			continue
		}
		// CONTAINMENT, before the join that would otherwise honour whatever
		// the pointer said.
		//
		// This is the only place in the fetch half that turns a declared path
		// into a WRITE (MkdirAll + WriteFile below), and ref.Path arrives from
		// an envelope this process did not author: on a worker it comes from an
		// upstream stage's surrendered ResultEnvelope, which is a bare
		// json.Unmarshal away from the producing pod; in a pod (#3823) it comes
		// over the network from the dispatcher. Every other call site that
		// reads a declared relative path against a fixed root goes through the
		// #120 containment primitive — ArtifactPointer.Resolve does, and
		// artifact-pointer.schema.json exists so "a foreign-authored envelope
		// that would escape the journal fails at validate time, not only at
		// resolve time". A fetch that wrote first and let Resolve judge later
		// would place a new filesystem-write primitive AHEAD of that check,
		// with the escaped bytes already on disk by the time anything refused.
		//
		// ArtifactPointer.Validate is that check, without touching the disk:
		// no absolute path, no volume, no ".." traversal, and a syntactically
		// real digest. Refused HARD rather than skipped — an escaping path is
		// not a cache miss to fill in later, it is an envelope that must not be
		// acted on at all.
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("workerhost: refuse context pointer %q: %w", pointers[i].Name, err)
		}
		dest := filepath.Join(stagingRoot, filepath.FromSlash(ref.Path))
		if _, err := os.Stat(dest); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("workerhost: stat context blob %q: %w", ref.Path, err)
		}
		data, err := store.Get(ctx, ref.Digest)
		if err != nil {
			if errors.Is(err, blobstore.ErrNotFound) {
				continue
			}
			return fmt.Errorf("workerhost: fetch context blob %q from %s: %w", ref.Digest, store.Describe(), err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("workerhost: create context blob dir: %w", err)
		}
		// Written under the digest's own path, so a corrupted or truncated
		// fetch cannot masquerade as a different artifact: Resolve verifies the
		// digest against the bytes it reads, and a mismatch is an integrity
		// fault exactly as it would be for a locally written blob.
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return fmt.Errorf("workerhost: write context blob %q: %w", ref.Path, err)
		}
	}
	return nil
}
