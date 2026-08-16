// Command designstatus validates the controlled status marker on design documents.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const headerLines = 10

var (
	statusMarker = regexp.MustCompile(`(?i)^\s*(?:>\s*)?(?:-\s*)?(?:\*\*status:\*\*|\*\*status:|status:)\s*(?:\*\*)?([a-z]+)\b`)
	statuses     = map[string]struct{}{
		"draft":       {},
		"approved":    {},
		"implemented": {},
		"superseded":  {},
		"historical":  {},
	}
)

func main() {
	if err := validateTrees("docs/design", "docs/adr"); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "designstatus: %v\n", err)
		os.Exit(1)
	}
}

func validateTrees(roots ...string) error {
	var problems []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || strings.ToLower(filepath.Ext(path)) != ".md" {
				return nil
			}
			if err := validateDocument(path); err != nil {
				problems = append(problems, err.Error())
			}
			return nil
		})
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", root, err))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New(strings.Join(problems, "\n"))
}

func validateDocument(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for line := 1; line <= headerLines && scanner.Scan(); line++ {
		match := statusMarker.FindStringSubmatch(scanner.Text())
		if match == nil {
			continue
		}
		status := strings.ToLower(match[1])
		if _, ok := statuses[status]; !ok {
			return fmt.Errorf("%s:%d: unknown status %q", path, line, status)
		}
		return nil
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: read: %w", path, err)
	}
	return fmt.Errorf("%s: missing Status marker in first %d lines", path, headerLines)
}
