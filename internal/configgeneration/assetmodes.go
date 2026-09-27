package configgeneration

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/gooberassets"
)

// Add logical mode metadata to the archive itself, never as untracked files at
// extraction. This preserves GooberDigest on Windows without weakening strict
// tree verification. Existing mirrored metadata must match the captured bytes.
func (a *Archive) includeAssetSourceModes() error {
	existing := make(map[string]file, len(a.Files))
	total := 0
	for _, entry := range a.Files {
		existing[entry.Path] = entry
		total += len(entry.Data)
	}
	var additions []file
	for _, directory := range a.Files {
		source := filepath.FromSlash(directory.Path)
		if !directory.Directory || !gooberassets.IsSourceDir(source) || gooberassets.IsWithinSourceDir(filepath.Dir(source)) {
			continue
		}
		bundle := a.assetBundle(directory)
		metadataPath := directory.Path + gooberassets.SourceModeSuffix
		if metadata, ok := existing[metadataPath]; ok {
			if metadata.Directory {
				return errors.New("asset source mode metadata is not a regular file")
			}
			if err := gooberassets.ValidateSourceModeMetadata(bundle, metadata.Data); err != nil {
				return err
			}
			continue
		}
		data, err := bundle.SourceModeMetadata()
		if err != nil {
			return err
		}
		total += len(data)
		if total > MaxContentBytes || len(a.Files)+len(additions)+1 > MaxFiles {
			return errors.New("config generation source mode metadata exceeds archive limits")
		}
		additions = append(additions, file{Path: metadataPath, Mode: 0600, Data: data})
	}
	a.Files = append(a.Files, additions...)
	return nil
}

func (a *Archive) assetBundle(directory file) *gooberassets.Bundle {
	wire := &gooberassets.WireBundle{RootMode: fs.ModeDir | fs.FileMode(directory.Mode)}
	for _, entry := range a.Files {
		relative, ok := strings.CutPrefix(entry.Path, directory.Path+"/")
		if !ok {
			continue
		}
		mode := fs.FileMode(entry.Mode)
		if entry.Directory {
			mode |= fs.ModeDir
		}
		wire.Entries = append(wire.Entries, gooberassets.WireEntry{Path: filepath.FromSlash(relative), Mode: mode, Data: entry.Data, Dir: entry.Directory})
	}
	// Match Load's sorted-child depth-first traversal, including prefix siblings.
	sort.Slice(wire.Entries, func(i, j int) bool {
		left := strings.Split(filepath.ToSlash(wire.Entries[i].Path), "/")
		right := strings.Split(filepath.ToSlash(wire.Entries[j].Path), "/")
		return slices.Compare(left, right) < 0
	})
	return gooberassets.FromWire(wire)
}
