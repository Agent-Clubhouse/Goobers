package instance

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/configsource"
	"github.com/goobers/goobers/internal/yamldoc"
)

// InvalidConfigScope describes the blast radius of a fail-closed (CFG-023)
// config directory load: the directory is admitted as one unit, so a single
// error-severity finding keeps every definition in it from loading (#5896).
type InvalidConfigScope struct {
	// InvalidFiles are config-relative paths carrying at least one
	// error-severity finding.
	InvalidFiles []string
	// BlockedFiles are config-relative definition files with no
	// error-severity finding of their own that still do not load because the
	// directory is rejected as a whole.
	BlockedFiles []ScopedFile
	// UnattributedErrors counts error-severity findings not tied to one file
	// (e.g. a missing Manifest), so BlockedFiles may also be implicated.
	UnattributedErrors int
}

// ScopedFile is one config definition file and the objects it declares.
type ScopedFile struct {
	Path    string
	Objects []string
}

// DescribeInvalidConfigScope partitions the definition files under dir by
// whether report attributes an error-severity finding to them.
func DescribeInvalidConfigScope(dir string, report *validate.Report) (InvalidConfigScope, error) {
	var scope InvalidConfigScope
	invalid := map[string]bool{}
	if report != nil {
		for _, issue := range report.Issues {
			if issue.Severity != validate.Error {
				continue
			}
			if issue.File == "" {
				scope.UnattributedErrors++
				continue
			}
			invalid[issue.File] = true
		}
	}
	for file := range invalid {
		scope.InvalidFiles = append(scope.InvalidFiles, file)
	}
	sort.Strings(scope.InvalidFiles)

	objects := map[string][]string{}
	err := configsource.WalkRawYAMLDocs(dir, func(_ string, rel string, doc yamldoc.ParsedDoc) error {
		rel = filepath.ToSlash(rel)
		if invalid[rel] {
			return nil
		}
		if _, ok := objects[rel]; !ok {
			objects[rel] = nil
		}
		if doc.Meta.Kind != "" && doc.Meta.Name != "" {
			objects[rel] = append(objects[rel], doc.Meta.Kind+"/"+doc.Meta.Name)
		}
		return nil
	})
	if err != nil {
		return scope, err
	}
	for path, objs := range objects {
		scope.BlockedFiles = append(scope.BlockedFiles, ScopedFile{Path: path, Objects: objs})
	}
	sort.Slice(scope.BlockedFiles, func(i, j int) bool {
		return scope.BlockedFiles[i].Path < scope.BlockedFiles[j].Path
	})
	return scope, nil
}

// Lines renders the scope as operator-facing text explaining that the whole
// directory is rejected and naming every file on each side of the split.
func (s InvalidConfigScope) Lines() []string {
	lines := []string{
		"the config directory is loaded as one unit (fail closed): no gaggle, goober, or workflow in it loads until every error is fixed",
		fmt.Sprintf("files with errors (%d):", len(s.InvalidFiles)),
	}
	for _, file := range s.InvalidFiles {
		lines = append(lines, "  "+file)
	}
	if s.UnattributedErrors > 0 {
		lines = append(lines, fmt.Sprintf("  (%d error(s) not attributed to a single file)", s.UnattributedErrors))
	}
	lines = append(lines, fmt.Sprintf("files without errors of their own, also not loaded (%d):", len(s.BlockedFiles)))
	for _, file := range s.BlockedFiles {
		line := "  " + file.Path
		if len(file.Objects) > 0 {
			line += " (" + strings.Join(file.Objects, ", ") + ")"
		}
		lines = append(lines, line)
	}
	return lines
}

// WriteInvalidConfigScope writes the scope of a rejected config directory to
// w, one line each; listing failures degrade to a one-line note.
func WriteInvalidConfigScope(w io.Writer, dir string, report *validate.Report) {
	scope, err := DescribeInvalidConfigScope(dir, report)
	if err != nil {
		_, _ = fmt.Fprintf(w, "note: the config directory is loaded as one unit; could not list its files: %v\n", err)
		return
	}
	for _, line := range scope.Lines() {
		_, _ = fmt.Fprintln(w, line)
	}
}
