package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/release/internal/imageinputs"
)

var prepareHarnessContext = imageinputs.PrepareHarness

func (plan *localImageBuild) buildHarnesses(images *imageContexts, baseDirectory string, base dockerImageDescription, evidence *localImageEvidence, stdout io.Writer) (err error) {
	evidence.HarnessSHA256 = make(map[string]string)
	// BuildKit treats a bare sha256 image ID in FROM as a remote repository.
	// Bind a unique local alias to the immutable ID and verify its identity
	// around every layer build. This alias is private to this build attempt.
	alias := "goobers-release-input:" + strings.TrimPrefix(base.ID, "sha256:") + "-" + filepath.Base(images.temporary)
	if err := requireAbsentImageTag(alias); err != nil {
		return err
	}
	if output, err := dockerImageCommand(30*time.Second, "image", "tag", base.ID, alias); err != nil {
		return fmt.Errorf("create local base-image alias: %w\n%s", err, output)
	}
	defer func() { err = errors.Join(err, removeOwnedImageAlias(alias, base.ID)) }()
	for _, harness := range []string{"copilot", "claude"} {
		directory, err := stageHarnessBuildInputs(images.temporary, harness, plan.platform.Arch)
		if err != nil {
			return err
		}
		evidence.HarnessSHA256[harness], err = sha256Hex(filepath.Join(directory, "SHA256SUMS"))
		if err != nil {
			return err
		}
		if err := requireImageIdentity(alias, base.ID); err != nil {
			return err
		}
		family := "goobers-harness-" + harness
		description, err := plan.buildImage(directory, family, []string{
			"--build-arg", "GOOBERS_BASE_IMAGE=" + alias, "--build-arg", "HARNESS=" + harness,
		}, stdout)
		if err != nil {
			return err
		}
		if err := verifyHarnessBase(description, base, alias); err != nil {
			return err
		}
		item, err := verifyBuiltImage(description, family, plan.tags[family], baseDirectory, evidence.Release)
		if err != nil {
			return err
		}
		item.BaseImageID = base.ID
		item.HarnessVersion, err = verifyImageHarness(description, harness)
		if err != nil {
			return err
		}
		evidence.Images = append(evidence.Images, item)
	}
	return nil
}

func stageHarnessBuildInputs(root, harness, arch string) (string, error) {
	repository, err := findAgentToolkitRepositoryRoot()
	if err != nil {
		return "", err
	}
	directory := filepath.Join(root, "harness-"+harness)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := prepareHarnessContext(ctx, harness, arch, directory); err != nil {
		return "", err
	}
	for _, name := range []string{"Dockerfile", ".dockerignore"} {
		data, err := os.ReadFile(filepath.Join(repository, "packaging", "docker", "harness", name))
		if err != nil {
			return "", fmt.Errorf("read harness image input %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o644); err != nil {
			return "", fmt.Errorf("write harness image input %s: %w", name, err)
		}
	}
	if err := writeImageContextTreeChecksums(directory); err != nil {
		return "", err
	}
	return directory, nil
}

func writeImageContextTreeChecksums(directory string) error {
	var lines []string
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported image context file type: %s", path)
		}
		digest, err := sha256Hex(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		lines = append(lines, digest+"  "+filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return fmt.Errorf("checksum harness input tree: %w", err)
	}
	sort.Strings(lines)
	return os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func requireImageIdentity(reference, expected string) error {
	description, err := inspectLocalImage(reference)
	if err != nil {
		return err
	}
	if description.ID != expected {
		return fmt.Errorf("local base-image alias %s changed from %s to %s", reference, expected, description.ID)
	}
	return nil
}

func removeOwnedImageAlias(alias, expected string) error {
	if err := requireImageIdentity(alias, expected); err != nil {
		return err // Do not remove a tag that another process has replaced.
	}
	if output, err := dockerImageCommand(30*time.Second, "image", "rm", alias); err != nil {
		return fmt.Errorf("remove temporary image alias %s: %w\n%s", alias, err, output)
	}
	return nil
}

func verifyHarnessBase(description, base dockerImageDescription, alias string) error {
	if err := requireImageIdentity(alias, base.ID); err != nil {
		return err
	}
	if len(base.RootFS.Layers) == 0 || len(description.RootFS.Layers) < len(base.RootFS.Layers) {
		return fmt.Errorf("harness image %s does not inherit the verified base layers", description.ID)
	}
	for index, layer := range base.RootFS.Layers {
		if description.RootFS.Layers[index] != layer {
			return fmt.Errorf("harness image %s has a different base layer at index %d", description.ID, index)
		}
	}
	return nil
}
