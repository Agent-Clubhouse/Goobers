// Command actionpins rejects mutable GitHub Actions references in
// repository workflows.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var actionReference = regexp.MustCompile(`^\s*(?:-\s*)?uses:\s*([^\s#]+)`)
var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

const workflowsDir = ".github/workflows"

func main() {
	if err := verify("."); err != nil {
		fmt.Fprintf(os.Stderr, "actionpins: %v\n", err)
		os.Exit(1)
	}
}

func verify(root string) error {
	files, err := workflowFiles(root)
	if err != nil {
		return err
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			relativePath = path
		}
		for lineNumber, line := range strings.Split(string(data), "\n") {
			match := actionReference.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			reference := match[1]
			if !isPinnedActionReference(reference) {
				return fmt.Errorf("%s:%d: action %q must use a full 40-character commit SHA", relativePath, lineNumber+1, reference)
			}
		}
	}
	return nil
}

// workflowFiles discovers every workflow in root's .github/workflows
// directory, both .yml and .yaml, so newly added workflow files are covered
// without this list needing to be updated by hand.
func workflowFiles(root string) ([]string, error) {
	var files []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(workflowsDir), pattern))
		if err != nil {
			return nil, fmt.Errorf("glob %s: %w", pattern, err)
		}
		files = append(files, matches...)
	}
	sort.Strings(files)
	return files, nil
}

func isPinnedActionReference(reference string) bool {
	if strings.HasPrefix(reference, "./") {
		return true
	}
	action, sha, ok := strings.Cut(reference, "@")
	return ok && action != "" && commitSHA.MatchString(sha)
}
