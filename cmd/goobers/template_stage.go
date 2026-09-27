package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/gaggletemplate"
	"github.com/goobers/goobers/internal/instance"
)

func validateTemplatePackage(name string, tree gaggletemplate.Tree) error {
	root, err := os.MkdirTemp("", "goobers-template-package-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(root) }()
	configDir := filepath.Join(root, "config")
	if err := tree.Write(filepath.Join(configDir, "gaggles", name)); err != nil {
		return err
	}
	manifest := fmt.Sprintf("apiVersion: goobers.dev/v1alpha1\nkind: Manifest\nmetadata:\n  name: template-validation\nspec:\n  instance: {name: template-validation, environment: dev}\n  gaggles: [%s]\n", name)
	if err := os.WriteFile(filepath.Join(configDir, "manifest.yaml"), []byte(manifest), 0644); err != nil {
		return err
	}
	_, report, err := instance.LoadConfigDir(configDir)
	if err != nil {
		return fmt.Errorf("template must be self-contained and valid: %w; %s", err, validationIssueSummary(report))
	}
	for _, warning := range report.Warnings() {
		if warning.Code == validate.WarningMissingSkillPackage {
			return fmt.Errorf("template is not self-contained: %s", warning.Explanation)
		}
	}
	return nil
}

// stageTemplate validates the candidate in the complete destination context.
// Staging sits beside the source so a final rename stays on the same volume.
func stageTemplate(configDir, name string, files gaggletemplate.Tree, source gaggletemplate.Source,
	revision string, baseline gaggletemplate.Tree, manifest []byte,
) (string, error) {
	parent := filepath.Join(configDir, "gaggles")
	if err := os.MkdirAll(parent, 0755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(parent, ".template-candidate-")
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := files.Write(stage); err != nil {
		return "", err
	}
	if err := gaggletemplate.Save(stage, source, revision, baseline); err != nil {
		return "", err
	}
	if err := validateTemplateCandidate(configDir, name, stage, manifest); err != nil {
		return "", err
	}
	keep = true
	return stage, nil
}

func validateTemplateCandidate(configDir, name, candidate string, manifest []byte) error {
	tmp, err := os.MkdirTemp("", "goobers-template-validation-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	stagedConfig := filepath.Join(tmp, "config")
	if err := copyTemplateValidationTree(configDir, stagedConfig); err != nil {
		return err
	}
	destination := filepath.Join(stagedConfig, "gaggles", name)
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	if err := copyTemplateValidationTree(candidate, destination); err != nil {
		return err
	}
	if manifest != nil {
		if err := os.WriteFile(filepath.Join(stagedConfig, "manifest.yaml"), manifest, 0644); err != nil {
			return err
		}
	}
	for _, sibling := range []string{"skills", "goobers"} {
		path := filepath.Join(filepath.Dir(configDir), sibling)
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := copyTemplateValidationTree(path, filepath.Join(tmp, sibling)); err != nil {
			return err
		}
	}
	if _, report, err := instance.LoadConfigDir(stagedConfig); err != nil {
		return fmt.Errorf("template candidate is invalid: %w; %s", err, validationIssueSummary(report))
	}
	return nil
}

func copyTemplateValidationTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("template validation refuses link %s", path)
		}
		if path != source && entry.IsDir() && len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			return filepath.SkipDir
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("template validation refuses non-regular file %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, info.Mode().Perm())
	})
}
