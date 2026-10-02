package main

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strings"
)

const justificationDir = "test/cmdgoobersgrowth/justifications/"

func gitOutput(root string, args ...string) ([]byte, error) {
	output, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return output, nil
}

func commitRef(root, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("empty revision")
	}
	data, err := gitOutput(root, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	return strings.TrimSpace(string(data)), err
}

func checkPullRequest(root, baseRef, headRef string, stdout io.Writer) error {
	base, err := commitRef(root, baseRef)
	if err != nil {
		return err
	}
	head, err := commitRef(root, headRef)
	if err != nil {
		return err
	}
	ancestor, err := gitOutput(root, "merge-base", base, head)
	if err != nil {
		return err
	}
	mergeBase := strings.TrimSpace(string(ancestor))
	before, err := revisionCounts(root, mergeBase)
	if err != nil {
		return err
	}
	after, err := revisionCounts(root, head)
	if err != nil {
		return err
	}
	grows := false
	for _, dimension := range dimensions {
		delta := after[dimension] - before[dimension]
		grows = grows || delta > 0
		_, _ = fmt.Fprintf(stdout, "cmdgoobersgrowth: PR %s %+d (merge-base %d, head %d)\n", dimension, delta, before[dimension], after[dimension])
	}
	if !grows {
		return nil
	}
	reason, err := addedJustification(root, mergeBase, head)
	if err != nil {
		return err
	}
	if reason == "" {
		return fmt.Errorf("unjustified cmd/goobers growth: add a new %s<issue-or-branch>.md explaining this PR's growth; do not re-pin the shared snapshot", justificationDir)
	}
	_, _ = fmt.Fprintf(stdout, "cmdgoobersgrowth: reviewed-growth declaration %s\n", reason)
	return nil
}

// Count immutable PR objects, never the synthetic merge checkout or a mutable
// branch tip. A concurrent main addition must not be charged to this PR.
func revisionCounts(root, ref string) (map[string]int, error) {
	listing, err := gitOutput(root, "ls-tree", "-r", "-z", ref, "--", "cmd/goobers/")
	if err != nil {
		return nil, err
	}
	counts := map[string]int{dimensions[0]: 0, dimensions[1]: 0}
	for _, entry := range bytes.Split(listing, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		metadata, name, ok := strings.Cut(string(entry), "\t")
		if !ok {
			return nil, fmt.Errorf("malformed source tree entry")
		}
		if path.Dir(name) != "cmd/goobers" || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if !strings.HasPrefix(metadata, "100644 blob ") && !strings.HasPrefix(metadata, "100755 blob ") {
			return nil, fmt.Errorf("source file %s must be a regular file", name)
		}
		source, err := gitOutput(root, "show", ref+":"+name)
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

// Only a newly added regular Markdown file counts. Old or modified declarations
// cannot accidentally authorize unrelated future growth. Unique issue/branch
// names let independently reviewed PRs merge without editing one shared file.
func addedJustification(root, base, head string) (string, error) {
	changed, err := gitOutput(root, "diff", "--no-renames", "--diff-filter=A", "--name-only", "-z", base, head, "--", justificationDir)
	if err != nil {
		return "", err
	}
	for _, data := range bytes.Split(changed, []byte{0}) {
		name := string(data)
		if path.Dir(name) != strings.TrimSuffix(justificationDir, "/") || !strings.HasSuffix(name, ".md") {
			continue
		}
		entry, err := gitOutput(root, "ls-tree", head, "--", name)
		if err != nil {
			return "", err
		}
		if !bytes.HasPrefix(entry, []byte("100644 blob ")) && !bytes.HasPrefix(entry, []byte("100755 blob ")) {
			continue
		}
		content, err := gitOutput(root, "show", head+":"+name)
		if err != nil {
			return "", err
		}
		if len(bytes.TrimSpace(content)) > 0 {
			return name, nil
		}
	}
	return "", nil
}
