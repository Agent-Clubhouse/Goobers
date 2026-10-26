package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/goobers/goobers/internal/dslmigrate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimewiring"
)

// Plan every file before any write, so a permission refusal or unsupported
// version in a mixed cohort cannot leave a partially upgraded configuration.
func runFixDSL21(root string, set *instance.ConfigSet, write, allowPolicyExpansion bool, stdout, stderr io.Writer) int {
	sources := map[string]string{}
	for _, wf := range set.Workflows {
		source, found := set.WorkflowSource(wf.Spec.Gaggle, wf.Name)
		if found {
			sources[source] = fmt.Sprintf("Workflow/%s (%s)", wf.Name, source)
		}
	}
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var changes []dslmigrate.FileChange
	additions := 0
	failed := false
	for _, source := range paths {
		path := filepath.Join(root, "config", source)
		before, err := os.ReadFile(path)
		var result *dslmigrate.Result
		if err == nil {
			result, err = dslmigrate.Migrate(before, "2.1")
		}
		if errors.Is(err, dslmigrate.ErrAlreadyAtTarget) {
			continue
		}
		if err != nil {
			pf(stderr, "FIX %s: %v\n", sources[source], err)
			failed = true
			continue
		}
		for _, addition := range result.PolicyAdditions {
			pf(stdout, "POLICY ADD %s task %q: %s (requires --allow-policy-expansion with --write)\n", source, addition.Task, addition.Action)
			additions++
		}
		applyFixResult(path, source, sources[source], "2.1", result, false, stdout)
		changes = append(changes, dslmigrate.FileChange{Path: filepath.ToSlash(source), Before: result.Before, After: result.After})
	}
	if failed {
		pln(stderr, "FIX: migration planning failed; no workflow files written")
		return 1
	}
	if len(changes) == 0 {
		pln(stdout, "FIX: every workflow is already at the target dslVersion; nothing to migrate")
		return 0
	}
	if !write {
		return 0
	}
	if additions > 0 && !allowPolicyExpansion {
		pln(stderr, "FIX: new declared policy actions require explicit --allow-policy-expansion; no workflow files written")
		return 1
	}
	if err := dslmigrate.WriteWithBackup(root, changes); err != nil {
		pf(stderr, "FIX: %v\n", err)
		return 1
	}
	pf(stdout, "FIX: %d workflows migrated to dslVersion 2.1 (written); backup: %s; restore with goobers fix --rollback %s\n", len(changes), filepath.Join(root, dslmigrate.BackupName), root)
	return 0
}

// Rollback is a separate command mode: validate its flags before any mutation
// and share the same initialized-root checks used by the forward migration.
func runFixRollback(root, target string, instanceSchema, write, allowPolicyExpansion bool, stdout, stderr io.Writer) int {
	if target != "" || instanceSchema || write || allowPolicyExpansion {
		pf(stderr, "error: --rollback is a separate mode; do not combine it with migration flags\n")
		return 2
	}
	if err := runtimewiring.PrepareManualRoot(instance.NewLayout(root), stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if err := dslmigrate.Rollback(root); err != nil {
		pf(stderr, "error: rollback: %v\n", err)
		return 1
	}
	pln(stdout, "FIX: original workflow bytes restored; migration backup consumed")
	return 0
}
