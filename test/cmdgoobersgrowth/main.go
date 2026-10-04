// Command cmdgoobersgrowth ratchets non-test Go source lines and files in
// cmd/goobers. Physical lines include comments and blanks, across all build tags;
// child directories are separate packages and are not counted.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const baselinePath = "test/cmdgoobersgrowth/baseline.txt"

var dimensions = []string{"non-test-line-count", "non-test-file-count"}

type justification struct {
	target int
	reason string
}

type baseline struct {
	counts  map[string]int
	reasons map[string]justification
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("cmdgoobersgrowth", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", ".", "repository root")
	baseRef := flags.String("base-ref", "HEAD", "base revision for PR comparison or snapshot update")
	headRef := flags.String("head-ref", "", "PR head revision; compare with its merge-base against base-ref")
	update := flags.Bool("update", false, "record current counts; growth needs exact-target justification directives")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	var err error
	switch {
	case *update:
		err = updateBaseline(*root, *baseRef, stdout)
	case *headRef != "":
		err = checkPullRequest(*root, *baseRef, *headRef, stdout)
	default:
		err = reportCounts(*root, stdout)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cmdgoobersgrowth: %v\n", err)
		return 1
	}
	return 0
}

// Without an explicit PR head, counts are informational. Main may contain
// independently reviewed growth from several merges beyond the last snapshot.
func reportCounts(root string, stdout io.Writer) error {
	counts, err := scanPackage(root)
	if err != nil {
		return err
	}
	for _, dimension := range dimensions {
		_, _ = fmt.Fprintf(stdout, "cmdgoobersgrowth: %s %d (informational snapshot; PR deltas are enforced)\n", dimension, counts[dimension])
	}
	return nil
}

func updateBaseline(root, baseRef string, stdout io.Writer) error {
	counts, err := scanPackage(root)
	if err != nil {
		return err
	}
	path := filepath.Join(root, baselinePath)
	candidate, err := readBaseline(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	previous, err := committedBaseline(root, baseRef)
	if err != nil {
		return err
	}
	candidate.counts = counts
	if err := validateUpdate(previous, candidate); err != nil {
		return err
	}
	if err := writeBaseline(path, candidate); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "cmdgoobersgrowth: wrote %s\n", baselinePath)
	return nil
}

func scanPackage(root string) (map[string]int, error) {
	path := filepath.Join(root, "cmd", "goobers")
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{dimensions[0]: 0, dimensions[1]: 0}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("source file %s must not be a symlink", entry.Name())
		}
		source, err := os.ReadFile(filepath.Join(path, entry.Name()))
		if err != nil {
			return nil, err
		}
		lines := bytes.Count(source, []byte{'\n'})
		if len(source) > 0 && source[len(source)-1] != '\n' {
			lines++
		}
		counts[dimensions[0]] += lines
		counts[dimensions[1]]++
	}
	return counts, nil
}

func readBaseline(path string) (baseline, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return baseline{}, err
	}
	return parseBaseline(content)
}

func parseBaseline(content []byte) (baseline, error) {
	base := baseline{counts: make(map[string]int), reasons: make(map[string]justification)}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := parseDirective(line, &base); err != nil {
			return baseline{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		return baseline{}, err
	}
	for _, dimension := range dimensions {
		if _, ok := base.counts[dimension]; !ok {
			return baseline{}, fmt.Errorf("missing !%s in %s", dimension, baselinePath)
		}
	}
	return base, nil
}

func parseDirective(line string, base *baseline) error {
	fields := strings.Fields(line)
	for _, dimension := range dimensions {
		switch fields[0] {
		case "!" + dimension:
			if len(fields) != 2 {
				return fmt.Errorf("%s wants one non-negative integer", fields[0])
			}
			count, err := strconv.Atoi(fields[1])
			if err != nil || count < 0 {
				return fmt.Errorf("invalid count in %q", line)
			}
			if _, exists := base.counts[dimension]; exists {
				return fmt.Errorf("duplicate %s", fields[0])
			}
			base.counts[dimension] = count
			return nil
		case "!" + dimension + "-justification":
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) != 3 || strings.TrimSpace(parts[2]) == "" {
				return fmt.Errorf("%s wants \\t<target>\\t<why>", fields[0])
			}
			target, err := strconv.Atoi(parts[1])
			if err != nil || target < 0 {
				return fmt.Errorf("invalid justification target in %q", line)
			}
			if _, exists := base.reasons[dimension]; exists {
				return fmt.Errorf("duplicate %s", fields[0])
			}
			base.reasons[dimension] = justification{target, strings.TrimSpace(parts[2])}
			return nil
		}
	}
	return fmt.Errorf("unknown baseline directive %q", line)
}

func committedBaseline(root, ref string) (*baseline, error) {
	if ref == "" {
		return nil, errors.New("base-ref must not be empty")
	}
	if err := exec.Command("git", "-C", root, "rev-parse", "--verify", ref+"^{commit}").Run(); err != nil {
		return nil, fmt.Errorf("resolve base-ref %q: %w", ref, err)
	}
	// A missing file is allowed only when introducing this gate. Invalid refs,
	// unreadable objects, and malformed historical baselines fail closed.
	listing, err := exec.Command("git", "-C", root, "ls-tree", "--name-only", ref, "--", baselinePath).Output()
	if err != nil {
		return nil, fmt.Errorf("locate baseline at %s: %w", ref, err)
	}
	if len(bytes.TrimSpace(listing)) == 0 {
		return nil, nil
	}
	content, err := exec.Command("git", "-C", root, "show", ref+":"+baselinePath).Output()
	if err != nil {
		return nil, fmt.Errorf("read baseline at %s: %w", ref, err)
	}
	base, err := parseBaseline(content)
	if err != nil {
		return nil, fmt.Errorf("parse baseline at %s: %w", ref, err)
	}
	return &base, nil
}

func validateUpdate(previous *baseline, next baseline) error {
	if previous == nil {
		return nil
	}
	var problems []string
	for _, dimension := range dimensions {
		current, old := next.counts[dimension], previous.counts[dimension]
		if current <= old {
			continue
		}
		reason, ok := next.reasons[dimension]
		if ok && reason.target == current && strings.TrimSpace(reason.reason) != "" {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s %s: baseline %d, new value %d; re-pin requires !%s-justification\\t%d\\t<why> and `make cmdgoobers-growth-update`", baselinePath, dimension, old, current, dimension, current))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

func writeBaseline(path string, base baseline) error {
	var content strings.Builder
	content.WriteString("# cmd/goobers growth ratchet (#5296). Generated by `make cmdgoobers-growth-update`.\n")
	content.WriteString("# Physical lines (including blanks/comments) in direct non-test .go files, all build tags.\n")
	content.WriteString("# Informational snapshot; PR enforcement compares head against merge-base.\n")
	content.WriteString("# Snapshot updates require !<dimension>-justification\\t<exact target>\\t<why>.\n")
	for _, dimension := range dimensions {
		count := base.counts[dimension]
		if reason, ok := base.reasons[dimension]; ok && reason.target == count {
			fmt.Fprintf(&content, "!%s-justification\t%d\t%s\n", dimension, reason.target, reason.reason)
		}
		fmt.Fprintf(&content, "!%s %d\n", dimension, count)
	}
	return os.WriteFile(path, []byte(content.String()), 0o644)
}
