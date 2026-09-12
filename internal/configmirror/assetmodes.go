package configmirror

import (
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/gooberassets"
)

// Preserve logical asset modes on every platform. A Windows worker cannot
// reconstruct Unix executable bits from Stat, but its pinned kit must retain
// the daemon's original fingerprint and transport those modes to stage pods.
func (s *Snapshot) preserveAssetModes(destination string) error {
	for _, directory := range s.archive.File {
		name := strings.TrimSuffix(directory.Name, "/")
		source := filepath.Join(destination, filepath.FromSlash(name))
		if !directory.Mode().IsDir() || !gooberassets.IsSourceDir(source) || gooberassets.IsWithinSourceDir(filepath.Dir(source)) {
			continue
		}
		modes := map[string]fs.FileMode{".": directory.Mode()}
		for _, entry := range s.archive.File {
			if relative, ok := strings.CutPrefix(entry.Name, directory.Name); ok && relative != "" {
				modes[strings.TrimSuffix(relative, "/")] = entry.Mode()
			}
		}
		if err := gooberassets.WriteSourceModes(source, modes); err != nil {
			return err
		}
	}
	return nil
}
