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
func MaterializeContext(ctx context.Context, store blobstore.Store, stagingRoot string, pointers []apiv1.ContextPointer) error {
	if store == nil || stagingRoot == "" || len(pointers) == 0 {
		return nil
	}
	for i := range pointers {
		ref := pointers[i].Artifact
		if ref == nil || ref.Path == "" || ref.Digest == "" {
			continue
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
