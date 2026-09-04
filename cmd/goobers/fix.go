package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/goobers/goobers/internal/dslmigrate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

const fixHelp = "Usage: goobers fix --to <version> [--write] [path]\n\n" +
	"Mechanically migrate every workflow in a config directory (default path\n" +
	"\".\") from its current dslVersion to <version>, one registered version\n" +
	"step at a time (DVL-6). Prints a reviewable unified diff per changed\n" +
	"workflow file by default; --write applies the diff to each file in\n" +
	"place instead. Refuses a workflow that is already at <version>, and\n" +
	"refuses any jump for which no direct one-step migration is registered —\n" +
	"chain multiple `fix` invocations for a multi-step upgrade, never a\n" +
	"silent multi-step rewrite. Never runs automatically; this is always an\n" +
	"author-run, reviewable change. Exit codes: 0 = migrated (or nothing to\n" +
	"migrate), 1 = one or more workflows could not be migrated,\n" +
	"2 = usage/IO error.\n"

func runFix(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("fix", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", "", "target dslVersion to migrate every workflow to (required)")
	write := fs.Bool("write", false, "apply the migration to each file in place (default: print a diff only)")
	fs.Usage = helpUsage(stderr, "fix")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	target := strings.TrimSpace(*to)
	if target == "" {
		pf(stderr, "error: --to <version> is required\n")
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	layout := instance.NewLayout(root)
	configDir := layout.ConfigDir()
	set, report, err := instance.LoadConfigDirForComparison(configDir)
	if err != nil {
		if !errors.Is(err, instance.ErrInvalidConfig) || set == nil {
			pf(stderr, "error: load config %s: %v\n", configDir, err)
			return 2
		}
		// A structurally invalid config still parses far enough to migrate
		// (dslVersion is unrelated to most validation failures) — fix should
		// not block on an unrelated pre-existing error the author will fix
		// separately; just surface it for visibility.
		if report != nil {
			printValidationIssues(stdout, report)
		}
		pf(stdout, "INVALID config %s; migrating anyway\n", configDir)
	}
	if len(set.Workflows) == 0 {
		pln(stdout, "FIX: no workflows found; nothing to migrate")
		return 0
	}

	ok := true
	migrated := 0
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		relSource, found := set.WorkflowSource(wf.Spec.Gaggle, wf.Name)
		if !found {
			continue
		}
		path := filepath.Join(configDir, relSource)
		label := fmt.Sprintf("Workflow/%s (%s)", wf.Name, relSource)

		source, err := os.ReadFile(path)
		if err != nil {
			pf(stdout, "FIX %s: %v\n", label, err)
			ok = false
			continue
		}
		result, err := dslmigrate.Migrate(source, target)
		if err != nil {
			if errors.Is(err, dslmigrate.ErrAlreadyAtTarget) {
				continue
			}
			pf(stdout, "FIX %s: %v\n", label, err)
			ok = false
			continue
		}
		if !result.Changed {
			continue
		}
		migrated++
		if *write {
			mode := os.FileMode(0o644)
			if fi, serr := os.Stat(path); serr == nil {
				mode = fi.Mode().Perm()
			}
			if err := journal.WriteFileAtomic(path, []byte(result.After), mode); err != nil {
				pf(stdout, "FIX %s: write: %v\n", label, err)
				ok = false
				continue
			}
			pf(stdout, "FIX %s: migrated to dslVersion %s (written)\n", label, target)
		} else {
			pf(stdout, "FIX %s: migrated to dslVersion %s (dry run — pass --write to apply)\n", label, target)
		}
		for _, note := range result.Notes {
			pf(stdout, "  %s\n", note)
		}
		diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A:        difflib.SplitLines(result.Before),
			B:        difflib.SplitLines(result.After),
			FromFile: "a/" + relSource,
			ToFile:   "b/" + relSource,
			Context:  3,
		})
		if err == nil {
			pf(stdout, "%s", diff)
		}
	}
	if !ok {
		return 1
	}
	if migrated == 0 {
		pln(stdout, "FIX: every workflow is already at the target dslVersion; nothing to migrate")
	}
	return 0
}
