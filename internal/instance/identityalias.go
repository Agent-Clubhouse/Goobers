package instance

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/journal"
)

// RootIdentityAliasesFileName stores historical IDs that resolve to the
// canonical .instance-id for this root.
const RootIdentityAliasesFileName = ".instance-id-aliases"

const (
	rootIdentityAliasesHeader = "goobers-root-identity-aliases-v1"
	maxIdentityAliasesBytes   = 4096
)

// ReadRootIdentityAliases reads and validates the root-scoped historical
// identity aliases without changing the root.
func ReadRootIdentityAliases(root string) ([]string, error) {
	path := filepath.Join(root, RootIdentityAliasesFileName)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("instance identity aliases are not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("instance identity aliases changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxIdentityAliasesBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxIdentityAliasesBytes {
		return nil, fmt.Errorf("instance identity aliases exceed bound")
	}
	parts := strings.Split(string(data), "\n")
	if len(parts) < 4 || parts[0] != rootIdentityAliasesHeader || parts[len(parts)-1] != "" {
		return nil, fmt.Errorf("invalid instance identity aliases")
	}
	canonical, err := ReadRootIdentity(root)
	if err != nil {
		return nil, fmt.Errorf("canonical instance identity is unavailable: %w", err)
	}
	if parts[1] != canonical {
		return nil, fmt.Errorf("instance identity aliases belong to a different canonical identity")
	}
	aliases := make([]string, 0, len(parts)-3)
	seen := map[string]struct{}{canonical: {}}
	for _, alias := range parts[2 : len(parts)-1] {
		if !ValidIdentity(alias) {
			return nil, fmt.Errorf("invalid instance identity alias")
		}
		if _, duplicate := seen[alias]; duplicate {
			return nil, fmt.Errorf("duplicate instance identity alias")
		}
		seen[alias] = struct{}{}
		aliases = append(aliases, alias)
	}
	if len(aliases) == 0 {
		return nil, fmt.Errorf("instance identity aliases contain no aliases")
	}
	return aliases, nil
}

// ResolveRootIdentity returns the canonical identity when value names either
// the canonical root or one of its persisted historical aliases.
func ResolveRootIdentity(root, value string) (string, error) {
	canonical, err := ReadRootIdentity(root)
	if err != nil {
		return "", err
	}
	if value == canonical {
		return canonical, nil
	}
	aliases, err := ReadRootIdentityAliases(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("instance identity %q is not recognized by this root", value)
	}
	if err != nil {
		return "", err
	}
	for _, alias := range aliases {
		if value == alias {
			return canonical, nil
		}
	}
	return "", fmt.Errorf("instance identity %q is not recognized by this root", value)
}

func ensureRootIdentityAlias(root, canonical, alias string) error {
	if alias == canonical {
		return nil
	}
	aliases, err := ReadRootIdentityAliases(root)
	if err == nil {
		for _, existing := range aliases {
			if existing == alias {
				return nil
			}
		}
		return fmt.Errorf("instance identity alias record does not contain legacy identity")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data := []byte(rootIdentityAliasesHeader + "\n" + canonical + "\n" + alias + "\n")
	return journal.WriteFileAtomic(filepath.Join(root, RootIdentityAliasesFileName), data, 0o644)
}
