package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/failfirst"
)

// checkFailFirstHelp documents `goobers check-fail-first` (#1214, TUT-A2): the
// deterministic stage the Tutor workflow runs after validate-config to
// mechanically enforce the fail-first validation-authorship contract
// (docs/design/tutor-redesign.md §5.1) — every gate a tutor-authored branch
// adds to a workflows/*.yaml file (a Workflow's Gates ARE its
// "validation/branching states") must be accompanied by evidence that it
// fails against the pre-change config and passes against the fix. A branch
// that adds no gate passes trivially; prose instructions alone cannot be the
// enforcement, only a gate that fails the run closed can.
const checkFailFirstHelp = "Usage: goobers check-fail-first [path]\n\n" +
	"Enforce TUT-A2's fail-first validation-authorship contract (#1214): any\n" +
	"new Workflow gate this run's branch adds under workflows/*.yaml must be\n" +
	"accompanied by fail-first evidence — a JSON file (default\n" +
	"fail-first-evidence.json, override with the evidenceFile input) proving\n" +
	"the new gate fails against the pre-change config and passes against the\n" +
	"post-change config. A branch that adds no gate passes trivially.\n" +
	"[path] defaults to the current directory (the stage's worktree).\n" +
	"Exit codes: 0 = no new gate, or every new gate has valid fail-first\n" +
	"evidence; 1 = a new gate lacks evidence; 2 = usage/IO error.\n"

func runCheckFailFirst(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("check-fail-first", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "check-fail-first")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}

	base := providerInput("base", providerBaseBranch())
	evidenceFile := providerInput("evidenceFile", "fail-first-evidence.json")

	newGates, err := detectNewGates(dir, base)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if len(newGates) == 0 {
		pf(stdout, "no new workflow gate on this branch; fail-first evidence not required\n")
		return 0
	}

	names := strings.Join(failfirst.GateRefNames(newGates), ", ")
	evidenceData, err := os.ReadFile(filepath.Join(dir, evidenceFile))
	if err != nil {
		pf(stderr, "error: this branch adds gate(s) %s but %s could not be read: %v\n", names, evidenceFile, err)
		return 1
	}
	var evidence failfirst.Evidence
	if err := json.Unmarshal(evidenceData, &evidence); err != nil {
		pf(stderr, "error: parse %s: %v\n", evidenceFile, err)
		return 1
	}
	if err := failfirst.VerifyEvidence(newGates, evidence); err != nil {
		pf(stderr, "error: fail-first validation-authorship contract (#1214): %v\n", err)
		return 1
	}

	pf(stdout, "fail-first evidence verified for gate(s): %s\n", names)
	return 0
}

// detectNewGates lists this branch's changed workflows/*.yaml files (relative
// to base) and reports every gate each adds, by diffing the file's gate names
// at base against its current content.
func detectNewGates(dir, base string) ([]failfirst.GateRef, error) {
	changed, err := checkFailFirstChangedFiles(dir, base)
	if err != nil {
		return nil, fmt.Errorf("compute changed files vs %q: %w", base, err)
	}
	var newGates []failfirst.GateRef
	for _, path := range changed {
		if !failfirst.IsWorkflowFile(path) {
			continue
		}
		oldContent, err := gitShowAtRef(dir, base, path)
		if err != nil {
			return nil, fmt.Errorf("read %s@%s: %w", path, base, err)
		}
		newContent, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		gates, err := failfirst.NewGates(path, oldContent, newContent)
		if err != nil {
			return nil, err
		}
		newGates = append(newGates, gates...)
	}
	return newGates, nil
}

// checkFailFirstChangedFiles lists the repo-relative paths this branch
// changes vs base (three-dot: the diff since the merge-base, i.e. the PR's
// file set), same semantics as changedFilesVsBase but scoped to dir so it is
// independently testable without depending on process cwd.
func checkFailFirstChangedFiles(dir, base string) ([]string, error) {
	cmd := exec.Command("git", "diff", "--no-renames", "--name-only", base+"...HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("git diff: %s: %w", strings.TrimSpace(string(ee.Stderr)), err)
		}
		return nil, fmt.Errorf("git diff: %w", err)
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// gitShowAtRef returns path's content at ref, or nil (no error) when ref does
// not have that path — treating the file as if it declared no gates, which is
// the fail-closed direction (a brand-new validation stage still needs
// evidence). Any other git failure IS still surfaced, closing the cycle
// rather than silently treating an unverifiable diff as "no gates."
func gitShowAtRef(dir, ref, path string) ([]byte, error) {
	cmd := exec.Command("git", "show", ref+":"+path)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.ToLower(stderr.String())
		if strings.Contains(msg, "does not exist") || strings.Contains(msg, "exists on disk, but not in") {
			return nil, nil
		}
		return nil, fmt.Errorf("git show %s:%s: %s: %w", ref, path, strings.TrimSpace(stderr.String()), err)
	}
	return out, nil
}
