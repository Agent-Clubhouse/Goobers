package artifactset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// JournalReader is a read-only resolver pinned to one run journal. Construct it
// in runner wiring, never from an agent-supplied directory. The opened root also
// prevents a concurrent symlink replacement from escaping containment.
type JournalReader struct{ root *os.Root }

// OpenJournal pins a read-only resolver to the runner-selected journal root.
func OpenJournal(root string) (*JournalReader, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	return &JournalReader{root: r}, nil
}

// Close releases the root handle after all checks using this reader complete.
func (r *JournalReader) Close() error { return r.root.Close() }

// ReadArtifact reads bounded regular-file content and verifies its size/digest.
func (r *JournalReader) ReadArtifact(ctx context.Context, pointer apiv1.ArtifactPointer, maxBytes int64) (data []byte, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 || maxBytes > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: invalid read bound", ErrInvalid)
	}
	if err := boundedPointer(pointer, maxBytes); err != nil {
		return nil, err
	}
	file, err := r.root.Open(pointer.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: missing artifact", ErrInvalid)
		}
		return nil, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			data = nil
			retErr = errors.Join(retErr, err)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != pointer.Size || info.Size() > maxBytes {
		return nil, fmt.Errorf("%w: artifact is not a bounded regular file", ErrInvalid)
	}
	data, err = io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) != pointer.Size || int64(len(data)) > maxBytes || apiv1.Digest(data) != pointer.Digest {
		return nil, fmt.Errorf("%w: artifact content mismatch", ErrInvalid)
	}
	return data, nil
}
