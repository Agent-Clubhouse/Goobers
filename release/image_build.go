package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	imagePrefixPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	imageTagPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	imageIDPattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	// Replaced by the behavioral fake engine in offline tests.
	dockerImageCommand = runDockerImageCommand
)

type localImageBuild struct {
	platform  Target
	tags      map[string]string
	attempted []string
}

type localImageEvidence struct {
	SchemaVersion  int                  `json:"schemaVersion"`
	Scope          string               `json:"scope"`
	Release        imageContextMetadata `json:"release"`
	EnginePlatform string               `json:"enginePlatform"`
	Native         bool                 `json:"native"`
	ContextSHA256  string               `json:"contextManifestSHA256"`
	HarnessSHA256  map[string]string    `json:"harnessManifestSHA256,omitempty"`
	Images         []builtImageEvidence `json:"images"`
}

type builtImageEvidence struct {
	Family         string                       `json:"family"`
	Reference      string                       `json:"reference"`
	ImageID        string                       `json:"imageID"`
	BaseImageID    string                       `json:"baseImageID,omitempty"`
	Platform       string                       `json:"platform"`
	User           string                       `json:"user"`
	BinarySHA256   map[string]string            `json:"binarySHA256"`
	VersionOutput  map[string]string            `json:"versionOutput"`
	HarnessVersion string                       `json:"harnessVersion,omitempty"`
	AdapterProbe   *copilotAdapterProbeEvidence `json:"adapterProbe,omitempty"`
	StageSmoke     *imageStageSmokeEvidence     `json:"stageSmoke"`
}

type dockerImageDescription struct {
	ID           string `json:"Id"`
	OS           string `json:"Os"`
	Architecture string `json:"Architecture"`
	Config       struct {
		User string `json:"User"`
	} `json:"Config"`
	RootFS struct {
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
}

func validateLocalImageOptions(opts options) error {
	if !opts.buildImages {
		if opts.imagePrefix != "" {
			return fmt.Errorf("-image-prefix requires -build-images")
		}
		return nil
	}
	if opts.imageContexts == "" {
		return fmt.Errorf("-build-images requires -image-contexts")
	}
	if !imagePrefixPattern.MatchString(opts.imagePrefix) {
		return fmt.Errorf("-build-images requires a lowercase repository prefix via -image-prefix (without a tag or digest)")
	}
	if opts.commit == "" || strings.Trim(opts.commit, "0123456789abcdef") != "" {
		return fmt.Errorf("local image builds require a hexadecimal embedded commit stamp")
	}
	for _, target := range opts.targets {
		if !imageTagPattern.MatchString(localImageTag(opts, target)) {
			return fmt.Errorf("version/commit/platform cannot form a Docker image tag of at most 128 characters")
		}
	}
	return nil
}

func localImageTag(opts options, target Target) string {
	return opts.version + "-" + opts.commit + "-" + target.OS + "-" + target.Arch
}

func imageFamilies(target Target) []string {
	if target.OS == "windows" {
		return []string{"goobers-base-windows"}
	}
	return []string{"goobers-base", "goobers-harness-copilot", "goobers-harness-claude"}
}

func prepareLocalImageBuild(opts options) (*localImageBuild, error) {
	if !opts.buildImages {
		return nil, nil
	}
	output, err := dockerImageCommand(30*time.Second, "info", "--format", "{{.OSType}}/{{.Architecture}}")
	if err != nil {
		return nil, fmt.Errorf("inspect native Docker engine: %w\n%s", err, output)
	}
	osName, architecture, ok := strings.Cut(strings.TrimSpace(string(output)), "/")
	if !ok {
		return nil, fmt.Errorf("image engine returned an invalid Docker platform")
	}
	switch architecture {
	case "x86_64":
		architecture = "amd64"
	case "aarch64":
		architecture = "arm64"
	}
	platform := Target{OS: osName, Arch: architecture}
	plan := &localImageBuild{platform: platform, tags: make(map[string]string)}
	for _, target := range opts.targets {
		if target != platform {
			return nil, fmt.Errorf("-build-images requires a matching native Docker engine: target %s, engine %s; prepare cross-platform inputs without -build-images", target, platform)
		}
		for _, family := range imageFamilies(target) {
			reference := opts.imagePrefix + "/" + family + ":" + localImageTag(opts, target)
			if err := requireAbsentImageTag(reference); err != nil {
				return nil, err
			}
			plan.tags[family] = reference
		}
	}
	return plan, nil
}

func requireAbsentImageTag(reference string) error {
	output, err := dockerImageCommand(30*time.Second, "image", "ls", "--quiet", "--no-trunc", "--filter", "reference="+reference)
	if err != nil {
		return fmt.Errorf("inspect local image tag %s: %w\n%s", reference, err, output)
	}
	if strings.TrimSpace(string(output)) != "" {
		return fmt.Errorf("local image tag already exists: %s; choose another -image-prefix to preserve existing images", reference)
	}
	return nil
}

func runDockerImageCommand(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}

func buildLocalReleaseImages(plan *localImageBuild, images *imageContexts, opts options, stdout io.Writer) error {
	if plan == nil {
		return nil
	}
	evidence, err := plan.build(images, opts, stdout)
	if err != nil {
		return fmt.Errorf("local image build/verification failed: %w; local image tags may remain: %s", err, strings.Join(plan.attempted, ", "))
	}
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return fmt.Errorf("encode local image evidence: %w", err)
	}
	if err := os.WriteFile(filepath.Join(images.temporary, "image-evidence.json"), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write local image evidence: %w", err)
	}
	images.built = true
	return nil
}

func (plan *localImageBuild) build(images *imageContexts, opts options, stdout io.Writer) (localImageEvidence, error) {
	evidence := localImageEvidence{
		SchemaVersion: 1, Scope: "local image build and native runtime verification; not published or signed",
		Release:        imageContextMetadata{1, "goobers-base-build-inputs", opts.version, opts.commit, opts.date, plan.platform.String()},
		EnginePlatform: plan.platform.String(), Native: true,
	}
	directory := filepath.Join(images.temporary, plan.platform.OS+"-"+plan.platform.Arch)
	manifest, err := sha256Hex(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		return evidence, err
	}
	evidence.ContextSHA256 = manifest
	family := imageFamilies(plan.platform)[0]
	base, err := plan.buildImage(directory, family, nil, stdout)
	if err != nil {
		return evidence, err
	}
	baseEvidence, err := verifyBuiltImage(base, family, plan.tags[family], directory, evidence.Release)
	if err != nil {
		return evidence, err
	}
	evidence.Images = append(evidence.Images, baseEvidence)
	if plan.platform.OS == "linux" {
		if err := plan.buildHarnesses(images, directory, base, &evidence, stdout); err != nil {
			return evidence, err
		}
	}
	return evidence, nil
}

func (plan *localImageBuild) buildImage(directory, family string, buildArgs []string, stdout io.Writer) (dockerImageDescription, error) {
	reference := plan.tags[family]
	if err := requireAbsentImageTag(reference); err != nil {
		return dockerImageDescription{}, err
	}
	_, _ = fmt.Fprintf(stdout, "image build %s (%s)\n", reference, plan.platform)
	plan.attempted = append(plan.attempted, reference)
	args := []string{"build", "--platform", plan.platform.String(), "--tag", reference}
	args = append(args, buildArgs...)
	args = append(args, directory)
	output, err := dockerImageCommand(30*time.Minute, args...)
	if err != nil {
		return dockerImageDescription{}, fmt.Errorf("build %s: %w\n%s", reference, err, output)
	}
	return inspectLocalImage(reference)
}

func inspectLocalImage(reference string) (dockerImageDescription, error) {
	output, err := dockerImageCommand(30*time.Second, "image", "inspect", reference)
	if err != nil {
		return dockerImageDescription{}, fmt.Errorf("inspect built image %s: %w\n%s", reference, err, output)
	}
	var descriptions []dockerImageDescription
	if err := json.Unmarshal(output, &descriptions); err != nil || len(descriptions) != 1 {
		return dockerImageDescription{}, fmt.Errorf("invalid Docker image inspection for %s", reference)
	}
	if !imageIDPattern.MatchString(descriptions[0].ID) {
		return dockerImageDescription{}, fmt.Errorf("image %s has no immutable Docker image ID", reference)
	}
	return descriptions[0], nil
}
