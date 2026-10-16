package secfile

import (
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/durability"
)

// WritePrivateAtomic publishes data by replacing path with a private sibling
// file. The sibling is protected before receiving secret bytes, including on
// Windows where permission bits alone do not restrict access. Failures before
// replacement preserve the old destination. Parent directories must exist.
func WritePrivateAtomic(path string, data []byte) error {
	f, err := createPrivateTemp(filepath.Dir(path))
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = durability.RemoveFile(tmp) }()
	// Keep the exclusive creation handle through write and sync. Reopening a
	// temporary pathname would permit replacement with a symlink before write.
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := VerifyPrivate(tmp); err != nil {
		return err
	}
	if err := durability.ReplaceFile(tmp, path); err != nil {
		return err
	}
	if err := VerifyPrivate(path); err != nil {
		_ = durability.RemoveFile(path)
		return err
	}
	return durability.SyncDir(filepath.Dir(path))
}
