package instance

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const instanceIdentityFile = "instance-id"

// ValidIdentity reports whether a value is a canonical nonzero instance ID.
// Transported identities follow the same contract as the durable root file.
func ValidIdentity(value string) bool {
	if len(value) != 32 || value == strings.Repeat("0", 32) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

// ReadIdentity reads the canonical root identity without creating or repairing
// state. The method remains as a compatibility wrapper for journal and provider
// attribution callers.
func (l Layout) ReadIdentity() (string, error) {
	if strings.TrimSpace(l.Root) == "" {
		return "", errors.New("instance identity requires an explicit root")
	}
	return ReadRootIdentity(l.Root)
}

// readLegacyIdentity reads the former journal-attribution identity. It is kept
// only for migration and telemetry correlation; new attribution uses
// .instance-id through ReadIdentity.
func readLegacyIdentity(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("instance identity requires an explicit root")
	}
	path := filepath.Join(root, instanceIdentityFile)
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
	if len(data) != 33 || !ValidIdentity(value) {
		return "", fmt.Errorf("invalid instance identity in %s; refusing to replace it", path)
	}
	return value, nil
}

// ReadLegacyIdentity reads the former journal-attribution identity without
// creating or repairing state. It exists so telemetry can correlate historical
// journals after migration.
func ReadLegacyIdentity(root string) (string, error) {
	return readLegacyIdentity(root)
}

// EnsureIdentity initializes the canonical .instance-id for journal and
// provider attribution. A lone legacy identity is adopted without rotation;
// when both historical IDs differ, the legacy value is retained as an alias.
func (l Layout) EnsureIdentity(ctx context.Context) (string, error) {
	if strings.TrimSpace(l.Root) == "" {
		return "", errors.New("instance identity requires an explicit root")
	}
	return ensureRootIdentity(ctx, l.Root, false)
}
