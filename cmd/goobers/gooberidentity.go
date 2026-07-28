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

func loadGooberSkillPackages(configDir string, goobers map[string]apiv1.GooberSpec) (map[string][]workflow.SkillFile, error) {
	names := map[string]struct{}{}
	for _, goober := range goobers {
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
		paths, ok, err := skillPackagePaths(configDir, name)
		if err != nil {
			return nil, fmt.Errorf("list skill %q package: %w", name, err)
		}
		if !ok {
			continue
		}
		root, _ := skillPackageDir(configDir, name)
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

func skillPackageDir(configDir, skill string) (string, bool) {
	if skill == "" || skill == "." || skill == ".." || strings.ContainsAny(skill, `/\`) || filepath.VolumeName(skill) != "" {
		return "", false
	}
	return filepath.Join(filepath.Dir(filepath.Clean(configDir)), "skills", skill), true
}

func skillPackagePaths(configDir, skill string) ([]string, bool, error) {
	root, ok := skillPackageDir(configDir, skill)
	if !ok {
		return nil, false, nil
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
		return nil, true, nil
	}
	if err != nil {
		return nil, true, err
	}
	sort.Strings(paths)
	return paths, true, nil
}

func compiledMachinesWithGooberDigests(
	configDir string,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	instructions map[string]string,
) (map[localscheduler.WorkflowIdentity]*workflow.Machine, map[localscheduler.WorkflowIdentity]string, error) {
	machines, digests, _, err := compiledMachinesWithGooberDigestsAndWarnings(configDir, set, goobers, instructions)
	return machines, digests, err
}

func compiledMachinesWithGooberDigestsAndWarnings(
	configDir string,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	instructions map[string]string,
) (map[localscheduler.WorkflowIdentity]*workflow.Machine, map[localscheduler.WorkflowIdentity]string, []gooberHarnessWarning, error) {
	machines, warnings, err := compiledMachinesWithWarnings(set, goobers)
	if err != nil {
		return nil, nil, nil, err
	}
	skillPackages, err := loadGooberSkillPackages(configDir, goobers)
	if err != nil {
		return nil, nil, nil, err
	}
	gooberDigests := make(map[localscheduler.WorkflowIdentity]string, len(machines))
	for identity, machine := range machines {
		digest, err := workflow.ComputeGooberDigest(machine.Def, goobers, instructions, skillPackages)
		if err != nil {
			return nil, nil, nil, &workflowDigestError{
				Gaggle:   identity.Gaggle,
				Workflow: identity.Workflow,
				Err:      err,
			}
		}
		gooberDigests[identity] = digest
	}
	return machines, gooberDigests, warnings, nil
}
