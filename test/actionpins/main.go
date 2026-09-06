// Command actionpins rejects mutable GitHub Actions references in privileged
// repository workflows.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var actionReference = regexp.MustCompile(`^\s*uses:\s*([^\s#]+)`)
var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

var workflowFiles = []string{
	".github/workflows/ci.yml",
	".github/workflows/release.yml",
}

func main() {
	if err := verify("."); err != nil {
		fmt.Fprintf(os.Stderr, "actionpins: %v\n", err)
		os.Exit(1)
	}
}

func verify(root string) error {
	for _, relativePath := range workflowFiles {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relativePath)))
		if err != nil {
			return fmt.Errorf("read %s: %w", relativePath, err)
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

func isPinnedActionReference(reference string) bool {
	if strings.HasPrefix(reference, "./") {
		return true
	}
	action, sha, ok := strings.Cut(reference, "@")
	return ok && action != "" && commitSHA.MatchString(sha)
}
