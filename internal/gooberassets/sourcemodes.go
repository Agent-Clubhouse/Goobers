package gooberassets

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const sourceModeSuffix = ".goobers-source-modes.json"
const sourceModeLimit = 4 << 20

type sourceModes struct {
	Version     int                    `json:"version"`
	Modes       map[string]fs.FileMode `json:"modes"`
	Fingerprint string                 `json:"fingerprint"`
}

// WriteSourceModes preserves archive modes separately from native filesystem
// permissions. The metadata binds every path, mode, and byte to one complete
// asset fingerprint; loading refuses changed content rather than trusting a
// stale identity. It is a sibling of assets, never materialized into a kit.
func WriteSourceModes(source string, modes map[string]fs.FileMode) error {
	bundle, err := scan(source, true)
	if err != nil {
		return err
	}
	if bundle == nil {
		return errors.New("source modes require an existing asset bundle")
	}
	if err := bundle.applySourceModes(modes); err != nil {
		return err
	}
	data, err := json.Marshal(sourceModes{Version: 1, Modes: modes, Fingerprint: bundle.Fingerprint()})
	if err != nil {
		return err
	}
	if len(data) > sourceModeLimit {
		return errors.New("asset source mode metadata exceeds limit")
	}
	f, err := os.OpenFile(source+sourceModeSuffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	return errors.Join(writeErr, syncErr, f.Close())
}

func restoreSourceModes(source string, bundle *Bundle) error {
	f, err := openAsset(source + sourceModeSuffix)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > sourceModeLimit {
		return errors.New("invalid asset source mode metadata")
	}
	data, err := io.ReadAll(io.LimitReader(f, sourceModeLimit+1))
	if err != nil {
		return err
	}
	var metadata sourceModes
	if len(data) > sourceModeLimit || json.Unmarshal(data, &metadata) != nil || metadata.Version != 1 {
		return errors.New("unsupported asset source mode metadata")
	}
	if err := bundle.applySourceModes(metadata.Modes); err != nil {
		return err
	}
	if bundle.Fingerprint() != metadata.Fingerprint {
		return errors.New("asset content does not match source mode fingerprint")
	}
	return nil
}

func (b *Bundle) applySourceModes(modes map[string]fs.FileMode) error {
	if len(modes) != len(b.entries)+1 || !validSourceMode(modes["."], true) {
		return errors.New("asset source modes do not describe the complete bundle")
	}
	b.rootMode = modes["."]
	for i := range b.entries {
		entry := &b.entries[i]
		mode, ok := modes[filepath.ToSlash(entry.path)]
		if !ok || !validSourceMode(mode, entry.dir) {
			return errors.New("invalid or missing asset source mode")
		}
		entry.mode = mode
	}
	return nil
}

func validSourceMode(mode fs.FileMode, directory bool) bool {
	return mode & ^(fs.ModePerm|fs.ModeDir) == 0 && mode.IsDir() == directory
}
