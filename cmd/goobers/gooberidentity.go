package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

type gooberInstructionsError struct {
	Goober string
	Err    error
}

func (e *gooberInstructionsError) Error() string {
	return fmt.Sprintf("read goober %q instructions: %v", e.Goober, e.Err)
}

func (e *gooberInstructionsError) Unwrap() error {
	return e.Err
}

type workflowDigestError struct {
	Gaggle   string
	Workflow string
	Err      error
}

func (e *workflowDigestError) Error() string {
	return fmt.Sprintf("digest workflow %q goobers: %v", e.Workflow, e.Err)
}

func (e *workflowDigestError) Unwrap() error {
	return e.Err
}

func loadGooberInstructions(configDir string, goobers map[string]apiv1.GooberSpec) (map[string]string, error) {
	names := make([]string, 0, len(goobers))
	for name := range goobers {
		names = append(names, name)
	}
	sort.Strings(names)
	instructions := make(map[string]string, len(goobers))
	for _, name := range names {
		content, err := os.ReadFile(instructionsPath(configDir, goobers[name], name))
		if err != nil {
			return nil, &gooberInstructionsError{Goober: name, Err: err}
		}
		instructions[name] = string(content)
	}
	return instructions, nil
}

func loadGooberSkillPackages(configDir, gaggle string, goobers map[string]apiv1.GooberSpec) (map[string][]workflow.SkillFile, error) {
	names := map[string]struct{}{}
	for _, goober := range goobers {
		if goober.Gaggle != gaggle {
			continue
		}
		for _, skill := range goober.Skills {
			names[skill] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	packages := make(map[string][]workflow.SkillFile, len(sorted))
	for _, name := range sorted {
		root, paths, ok, err := skillPackagePaths(configDir, gaggle, name)
		if err != nil {
			return nil, fmt.Errorf("list skill %q package: %w", name, err)
		}
		if !ok {
			continue
		}
		files := make([]workflow.SkillFile, 0, len(paths))
		for _, filePath := range paths {
			content, err := os.ReadFile(filePath)
			if err != nil {
				return nil, fmt.Errorf("read skill %q package file: %w", name, err)
			}
			relative, err := filepath.Rel(root, filePath)
			if err != nil {
				return nil, fmt.Errorf("resolve skill %q package file: %w", name, err)
			}
			files = append(files, workflow.SkillFile{
				Path:    filepath.ToSlash(relative),
				Content: string(content),
			})
		}
		packages[name] = files
	}
	return packages, nil
}

func skillPackageDirs(configDir, gaggle, skill string) (scoped, shared string, ok bool) {
	if skill == "" || skill == "." || skill == ".." || strings.ContainsAny(skill, `/\`) || filepath.VolumeName(skill) != "" {
		return "", "", false
	}
	configDir = filepath.Clean(configDir)
	return filepath.Join(configDir, "gaggles", gaggle, "skills", skill),
		filepath.Join(filepath.Dir(configDir), "skills", skill), true
}

func skillPackagePaths(configDir, gaggle, skill string) (string, []string, bool, error) {
	scoped, shared, ok := skillPackageDirs(configDir, gaggle, skill)
	if !ok {
		return "", nil, false, nil
	}
	root := shared
	if _, err := os.Stat(scoped); err == nil {
		root = scoped
	} else if !os.IsNotExist(err) {
		return "", nil, true, err
	}
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			paths = append(paths, path)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return root, nil, true, nil
	}
	if err != nil {
		return root, nil, true, err
	}
	sort.Strings(paths)
	return root, paths, true, nil
}

func compiledMachinesWithGooberDigestsAndWarnings(
	configDir string,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	instructions map[string]string,
	envPassthrough []string,
	harnessCommand map[string][]string,
	deferModelDiscovery bool,
) (map[localscheduler.WorkflowIdentity]*workflow.Machine, map[localscheduler.WorkflowIdentity]string, map[string]apiv1.GooberSpec, []gooberHarnessWarning, error) {
	machines, resolvedGoobers, warnings, err := compiledMachinesWithWarnings(set, goobers, envPassthrough, harnessCommand, deferModelDiscovery)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	gooberDigests := make(map[localscheduler.WorkflowIdentity]string, len(machines))
	for identity, machine := range machines {
		skillPackages, err := loadGooberSkillPackages(configDir, identity.Gaggle, resolvedGoobers)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		digest, err := workflow.ComputeGooberDigest(machine.Def, resolvedGoobers, instructions, skillPackages)
		if err != nil {
			return nil, nil, nil, nil, &workflowDigestError{
				Gaggle:   identity.Gaggle,
				Workflow: identity.Workflow,
				Err:      err,
			}
		}
		gooberDigests[identity] = digest
	}
	return machines, gooberDigests, resolvedGoobers, warnings, nil
}
