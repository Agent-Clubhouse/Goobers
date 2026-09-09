package configmirror

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const validationOwner = "goobers-config-mirror-validation-v1\n"

// The caller holds publish.lock. One fixed, explicitly owned validation tree
// bounds crash leftovers; a later publication reclaims it before any new copy.
func validateStagedSnapshot(ctx context.Context, destination, staged string, validate func(string) error) error {
	root := filepath.Join(destination, ".worker-config-validation")
	if err := reclaimValidation(root); err != nil {
		return err
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	marker := filepath.Join(root, ".owner")
	if err := os.WriteFile(marker, []byte(validationOwner), 0o600); err != nil {
		_ = os.Remove(root)
		return err
	}
	defer func() { _ = reclaimValidation(root) }()
	snapshot, err := openSnapshot(staged)
	if err != nil {
		return err
	}
	defer func() { _ = snapshot.Close() }()
	if err := snapshot.Extract(ctx, root); err != nil {
		return err
	}
	return validate(root)
}

func reclaimValidation(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("config mirror validation path is not a directory")
	}
	marker := filepath.Join(root, ".owner")
	info, err = os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		// A crash between mkdir and writing the marker can leave an empty
		// directory. Remove only if empty; never recursively delete unowned data.
		return os.Remove(root)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(validationOwner)) {
		return errors.New("config mirror validation ownership is invalid")
	}
	f, err := os.Open(marker)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, int64(len(validationOwner)+1)))
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if string(data) != validationOwner {
		return errors.New("config mirror validation directory is not owned")
	}
	return os.RemoveAll(root)
}
