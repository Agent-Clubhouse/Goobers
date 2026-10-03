package configsource

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/configtree"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/yamldoc"
)

// WalkRawYAMLDocs visits each YAML document in the config definition trees.
func WalkRawYAMLDocs(root string, fn func(path, rel string, doc yamldoc.ParsedDoc) error) error {
	opts := mcpio.DefaultWalkFilesOptions()
	opts.SkipDirPredicate = func(path string, _ fs.DirEntry) bool {
		return configtree.IsGaggleSkillsDir(root, path) || gooberassets.IsSourceDir(path)
	}
	opts.SkipSymlinkEntries = false

	err := configtree.WalkDefinitionTrees(root, func(tree string) error {
		return mcpio.WalkFiles(tree, func(path string, _ fs.DirEntry) error {
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".yaml" && ext != ".yml" {
				return nil
			}

			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			for _, doc := range yamldoc.SplitDocuments(raw) {
				if err := fn(path, rel, doc); err != nil {
					return err
				}
			}
			return nil
		}, opts)
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", root, err)
	}
	return nil
}
