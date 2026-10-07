package secfile

import (
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/durability"
)

// WritePrivateAtomic publishes data by replacing path with a private sibling
// file. The sibling is protected before receiving secret bytes, including on
// Windows where permission bits alone do not restrict access. Failures before
// replacement preserve the old destination. Parent directories must exist.
func WritePrivateAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".private-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = durability.RemoveFile(tmp) }()
	if err := f.Close(); err != nil {
		return err
	}
	if err := WritePrivate(tmp, data); err != nil {
		return err
	}
	if err := syncPrivateFile(tmp); err != nil {
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

func syncPrivateFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
