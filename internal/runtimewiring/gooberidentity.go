package runtimewiring

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

// GooberInstructionsError identifies the goober whose instructions could not be loaded.
type GooberInstructionsError struct {
	Goober string
	Err    error
}

func (e *GooberInstructionsError) Error() string {
	return fmt.Sprintf("read goober %q instructions: %v", e.Goober, e.Err)
}

func (e *GooberInstructionsError) Unwrap() error {
	return e.Err
}

// WorkflowDigestError identifies the workflow whose goober digest could not be computed.
type WorkflowDigestError struct {
	Gaggle   string
	Workflow string
	Err      error
}

func (e *WorkflowDigestError) Error() string {
	return fmt.Sprintf("digest workflow %q goobers: %v", e.Workflow, e.Err)
}

func (e *WorkflowDigestError) Unwrap() error {
	return e.Err
}

// LoadGooberInstructions loads instructions for each goober from the config tree.
func LoadGooberInstructions(configDir string, set *instance.ConfigSet, goobers map[string]apiv1.GooberSpec) (map[string]string, error) {
	names := make([]string, 0, len(goobers))
	for name := range goobers {
		names = append(names, name)
	}
	sort.Strings(names)
	instructions := make(map[string]string, len(goobers))
	for _, name := range names {
		content, err := os.ReadFile(ResolvedInstructionsPath(configDir, set, goobers[name], name))
		if err != nil {
			return nil, &GooberInstructionsError{Goober: name, Err: err}
		}
		instructions[name] = string(content)
	}
	return instructions, nil
}

// ResolvedInstructionsPath resolves a goober instruction path relative to its definition.
func ResolvedInstructionsPath(configDir string, set *instance.ConfigSet, spec apiv1.GooberSpec, gooberName string) string {
	return filepath.Join(ResolvedGooberDefinitionDir(configDir, set, spec, gooberName), spec.Instructions)
}

// ResolvedGooberDefinitionDir is the directory holding the goober's parsed
// definition file — the same directory validation resolves spec.instructions
// and the goober's assets against (#6611). It falls back to the conventional
// name-derived layout only when no loaded ConfigSet provenance is available.
func ResolvedGooberDefinitionDir(configDir string, set *instance.ConfigSet, spec apiv1.GooberSpec, gooberName string) string {
	if set != nil {
		if source, ok := set.GooberSource(gooberName); ok {
			return filepath.Join(configDir, filepath.Dir(source))
		}
	}
	return gooberDefinitionDir(configDir, spec, gooberName)
}

type skillSource struct{ Name, Gaggle string }

func gooberSkillSources(gaggle string, goobers map[string]apiv1.GooberSpec) map[string]skillSource {
	names := map[string]skillSource{}
	for _, goober := range goobers {
		if goober.Gaggle != "" && goober.Gaggle != gaggle {
			continue
		}
		for _, skill := range goober.Skills {
			key := skill
			if goober.Gaggle == "" {
				key = workflow.SharedSkillPackageKey(skill)
			}
			names[key] = skillSource{Name: skill, Gaggle: goober.Gaggle}
		}
	}
	return names
}

// LoadGooberSkillPackages loads the skill files available to each gaggle.
func LoadGooberSkillPackages(configDir, gaggle string, goobers map[string]apiv1.GooberSpec) (map[string][]workflow.SkillFile, error) {
	names := gooberSkillSources(gaggle, goobers)
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	packages := make(map[string][]workflow.SkillFile, len(sorted))
	for _, key := range sorted {
		name := names[key].Name
		root, paths, ok, err := skillPackagePaths(configDir, names[key].Gaggle, name)
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
		packages[key] = files
	}
	return packages, nil
}

// SkillPackageDirs returns the directories that contribute skill packages.
func SkillPackageDirs(configDir, gaggle, skill string) (scoped, shared string, ok bool) {
	if skill == "" || skill == "." || skill == ".." || strings.ContainsAny(skill, `/\`) || filepath.VolumeName(skill) != "" {
		return "", "", false
	}
	configDir = filepath.Clean(configDir)
	if gaggle == "" {
		shared := filepath.Join(filepath.Dir(configDir), "skills", skill)
		return shared, shared, true
	}
	return filepath.Join(configDir, "gaggles", gaggle, "skills", skill),
		filepath.Join(filepath.Dir(configDir), "skills", skill), true
}

func skillPackagePaths(configDir, gaggle, skill string) (string, []string, bool, error) {
	scoped, shared, ok := SkillPackageDirs(configDir, gaggle, skill)
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

// CompiledMachinesWithGooberDigestsAndWarnings compiles workflows and captures their persona digests and harness warnings.
func CompiledMachinesWithGooberDigestsAndWarnings(
	configDir string,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	instructions map[string]string,
	environment harness.EnvironmentConfig,
	harnessCommand map[string][]string,
	deferModelDiscovery bool,
	modelCredential func(ctx context.Context) (string, error),
	knownTelemetryConnectors []string,
) (map[localscheduler.WorkflowIdentity]*workflow.Machine, map[localscheduler.WorkflowIdentity]string, map[string]apiv1.GooberSpec, []GooberHarnessWarning, error) {
	return CompiledMachinesWithGooberDigests(
		set, goobers, instructions, environment, harnessCommand, deferModelDiscovery, modelCredential, knownTelemetryConnectors,
		func(gaggle string, resolved map[string]apiv1.GooberSpec) (map[string][]workflow.SkillFile, error) {
			return LoadGooberSkillPackages(configDir, gaggle, resolved)
		},
	)
}

// CompiledMachinesWithGooberDigests is the digest computation both the daemon
// and `goobers worker` run, with the ONE input that differs between them
// injected: where a gaggle's skill packages come from.
//
// The daemon reads them from the config directory on disk, because the tree it
// is compiling is the tree it is serving. The worker reads them from the
// in-memory config snapshot the digest is being computed FOR (#3884), because
// its comparison is "does snapshot S resolve the digest this run pinned" — and
// a snapshot that re-read skill files from disk would answer that question
// about whatever tree happens to be mounted right now, which is precisely the
// staleness the pin exists to detect.
//
// Everything else is shared on purpose: a second copy of the compile →
// admit → ComputeGooberDigest sequence is how the worker would start computing
// digests the daemon never mints, turning every pinned attempt into a refusal.
func CompiledMachinesWithGooberDigests(
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	instructions map[string]string,
	environment harness.EnvironmentConfig,
	harnessCommand map[string][]string,
	deferModelDiscovery bool,
	modelCredential func(ctx context.Context) (string, error),
	knownTelemetryConnectors []string,
	skillPackagesFor func(gaggle string, resolved map[string]apiv1.GooberSpec) (map[string][]workflow.SkillFile, error),
) (map[localscheduler.WorkflowIdentity]*workflow.Machine, map[localscheduler.WorkflowIdentity]string, map[string]apiv1.GooberSpec, []GooberHarnessWarning, error) {
	machines, resolvedGoobers, warnings, err := CompiledMachinesWithWarnings(set, goobers, environment, harnessCommand, deferModelDiscovery, modelCredential, knownTelemetryConnectors)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	gooberDigests, err := ComputeMachineGooberDigests(machines, resolvedGoobers, instructions, skillPackagesFor)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return machines, gooberDigests, resolvedGoobers, warnings, nil
}

// ComputeMachineGooberDigests computes each compiled workflow's effective persona digest.
func ComputeMachineGooberDigests(machines map[localscheduler.WorkflowIdentity]*workflow.Machine, resolvedGoobers map[string]apiv1.GooberSpec, instructions map[string]string, skillPackagesFor func(string, map[string]apiv1.GooberSpec) (map[string][]workflow.SkillFile, error)) (map[localscheduler.WorkflowIdentity]string, error) {
	gooberDigests := make(map[localscheduler.WorkflowIdentity]string, len(machines))
	for identity, machine := range machines {
		skillPackages, err := skillPackagesFor(identity.Gaggle, resolvedGoobers)
		if err != nil {
			return nil, err
		}
		digest, err := workflow.ComputeGooberDigest(machine.Def, resolvedGoobers, instructions, skillPackages)
		if err != nil {
			return nil, &WorkflowDigestError{
				Gaggle:   identity.Gaggle,
				Workflow: identity.Workflow,
				Err:      err,
			}
		}
		gooberDigests[identity] = digest
	}
	return gooberDigests, nil
}
