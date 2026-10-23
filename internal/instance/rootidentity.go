package instance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/journal"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// RootIdentityFileName stores the durable, opaque instance-root identifier.
const RootIdentityFileName = ".instance-id"

// ReadRootIdentity is observational: a legacy root without an identity remains
// unidentified until initialization or another authorized mutation adopts it.
// The ID is an opaque 128-bit value, not a daemon liveness signal.
func ReadRootIdentity(root string) (string, error) {
	path := filepath.Join(root, RootIdentityFileName)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("instance identity is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, opened) {
		return "", fmt.Errorf("instance identity changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, 34))
	if err != nil {
		return "", err
	}
	if len(data) != 33 || data[32] != '\n' {
		return "", fmt.Errorf("invalid instance identity encoding")
	}
	id := string(data[:32])
	if !ValidIdentity(id) {
		return "", fmt.Errorf("invalid instance identity encoding")
	}
	return id, nil
}

// EnsureRootIdentity creates one durable ID for an existing provisioned root.
// A lock serializes first adoption; a corrupt existing ID is never overwritten.
func EnsureRootIdentity(ctx context.Context, root string) (string, error) {
	return ensureRootIdentity(ctx, root, true)
}

// InstallRootIdentity installs an externally authoritative identity into a
// private empty root. It never invents or replaces an identity.
func InstallRootIdentity(ctx context.Context, root, id string) error {
	if !ValidIdentity(id) {
		return errors.New("invalid authoritative instance identity")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	handle, err := acquireRootIdentityLock(ctx, filepath.Join(root, RootIdentityFileName+".lock"))
	if err != nil {
		return err
	}
	defer func() { _ = handle.Release() }()
	existing, err := ReadRootIdentity(root)
	switch {
	case err == nil && existing == id:
		return nil
	case err == nil:
		return errors.New("worker root belongs to a different instance")
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	return journal.WriteFileAtomic(filepath.Join(root, RootIdentityFileName), []byte(id+"\n"), 0o644)
}

func ensureRootIdentity(ctx context.Context, root string, requireConfig bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	handle, err := acquireRootIdentityLock(ctx, filepath.Join(root, RootIdentityFileName+".lock"))
	if err != nil {
		return "", err
	}
	defer func() { _ = handle.Release() }()

	canonical, canonicalErr := ReadRootIdentity(root)
	if canonicalErr != nil && !errors.Is(canonicalErr, os.ErrNotExist) {
		return "", canonicalErr
	}
	legacy, legacyErr := readLegacyIdentity(root)
	if legacyErr != nil && !errors.Is(legacyErr, os.ErrNotExist) {
		return "", legacyErr
	}
	if _, aliasErr := ReadRootIdentityAliases(root); aliasErr != nil && !errors.Is(aliasErr, os.ErrNotExist) {
		return "", aliasErr
	}
	if canonicalErr == nil {
		if legacyErr == nil {
			if err := ensureRootIdentityAlias(root, canonical, legacy); err != nil {
				return "", err
			}
		}
		return canonical, nil
	}
	if legacyErr == nil {
		if err := journal.WriteFileAtomic(filepath.Join(root, RootIdentityFileName), []byte(legacy+"\n"), 0o644); err != nil {
			return "", err
		}
		return legacy, nil
	}
	if _, err := os.Lstat(filepath.Join(root, RootDecommissionFileName)); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("cannot adopt an unidentified historical root; restore its original identity")
	}
	if requireConfig {
		if info, err := os.Stat(NewLayout(root).ConfigFile()); err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("instance identity requires an existing instance.yaml")
		}
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random[:])
	if err := journal.WriteFileAtomic(filepath.Join(root, RootIdentityFileName), []byte(id+"\n"), 0o644); err != nil {
		return "", err
	}
	return id, nil
}

func acquireRootIdentityLock(ctx context.Context, path string) (*platformlock.Handle, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		handle, err := platformlock.TryAcquire(path)
		if !errors.Is(err, platformlock.ErrHeld) {
			return handle, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
