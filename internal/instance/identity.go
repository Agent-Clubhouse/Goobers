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
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/lock"
)

const instanceIdentityFile = "instance-id"

// ReadIdentity reads an existing root identity without creating or repairing
// state. Missing legacy identity and corrupt identity are distinct errors;
// neither may be presented as a verified instance identity.
func (l Layout) ReadIdentity() (string, error) {
	if strings.TrimSpace(l.Root) == "" {
		return "", errors.New("instance identity requires an explicit root")
	}
	path := filepath.Join(l.Root, instanceIdentityFile)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() != 33 {
		return "", fmt.Errorf("invalid instance identity file %s: expected a regular 33-byte file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 34))
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(string(data), "\n")
	decoded, decodeErr := hex.DecodeString(value)
	if len(data) != 33 || len(value) != 32 || len(decoded) != 16 || decodeErr != nil ||
		value != strings.ToLower(value) || value == strings.Repeat("0", 32) {
		return "", fmt.Errorf("invalid instance identity in %s; refusing to replace it", path)
	}
	return value, nil
}

// EnsureIdentity initializes one random, durable identity for a root. Gaggle
// layouts share their instance root's identity. Renaming a root preserves its
// identity; creating a different root does not inherit it from a display name.
// Readers never see a partially written identity. Corruption fails closed
// rather than silently rotating an identity already referenced by journals.
func (l Layout) EnsureIdentity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if value, err := l.ReadIdentity(); err == nil || !errors.Is(err, os.ErrNotExist) {
		return value, err
	}
	held, err := l.lockIdentity(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = held.Release() }()
	// A concurrent initializer may have published while this caller waited.
	if value, err := l.ReadIdentity(); err == nil || !errors.Is(err, os.ErrNotExist) {
		return value, err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate instance identity: %w", err)
	}
	value := hex.EncodeToString(random[:])
	if err := l.publishIdentity(value); err != nil {
		return "", err
	}
	return value, nil
}

func (l Layout) lockIdentity(ctx context.Context) (*lock.Handle, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		held, err := lock.TryAcquire(filepath.Join(l.Root, ".instance-id.lock"))
		if !errors.Is(err, lock.ErrHeld) {
			return held, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for instance identity initialization: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (l Layout) publishIdentity(value string) error {
	// One fixed pending file under the initialization lock bounds crash
	// leftovers to one file; the next initializer reclaims it. This is not an
	// unbounded directory of temporary identities, nor operator configuration.
	pending := filepath.Join(l.Root, ".instance-id.pending")
	if info, err := os.Lstat(pending); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("instance identity pending path is not a regular file: %s", pending)
		}
		if err := os.Remove(pending); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(pending) }()
	_, writeErr := io.WriteString(f, value+"\n")
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return fmt.Errorf("persist instance identity: %w", err)
	}
	if err := durability.ReplaceFile(pending, filepath.Join(l.Root, instanceIdentityFile)); err != nil {
		return err
	}
	return durability.SyncDir(l.Root)
}
