package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeImageEngine struct {
	t                  *testing.T
	platform           Target
	tags               map[string]string
	images             map[string]dockerImageDescription
	metadata           map[string]imageContextMetadata
	digests            map[string]map[string]string
	families           map[string]string
	commands           [][]string
	badHashFamily      string
	badStamp           bool
	wrongBase          bool
	copilotProbeOutput string
	copilotProbeCode   int
}

type imageProbeExitError struct{ code int }

func (e imageProbeExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e imageProbeExitError) ExitCode() int { return e.code }

func useFakeImageEngine(t *testing.T, target Target) *fakeImageEngine {
	t.Helper()
	engine := &fakeImageEngine{
		t: t, platform: target, tags: make(map[string]string), images: make(map[string]dockerImageDescription),
		metadata: make(map[string]imageContextMetadata), digests: make(map[string]map[string]string), families: make(map[string]string),
	}
	original, originalPrepare := dockerImageCommand, prepareHarnessContext
	dockerImageCommand = engine.run
	prepareHarnessContext = func(_ context.Context, harness, _ string, directory string) error {
		if err := os.MkdirAll(filepath.Join(directory, "package"), 0o755); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(directory, "bin"), 0o755); err != nil {
			return err
		}
		for name, contents := range map[string]string{"package/payload": "pinned native fixture", "bin/" + harness: "launcher", "version": "1.2.3", "source": "verified public fixture"} {
			if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	t.Cleanup(func() { dockerImageCommand, prepareHarnessContext = original, originalPrepare })
	return engine
}

func imageArgument(args []string, flag string) string {
	for index, arg := range args {
		if arg == flag && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func (engine *fakeImageEngine) run(_ time.Duration, args ...string) ([]byte, error) {
	engine.commands = append(engine.commands, append([]string(nil), args...))
	switch args[0] {
	case "info":
		return []byte(engine.platform.String()), nil
	case "build":
		return engine.build(args)
	case "run":
		return engine.probe(args)
	case "image":
		return engine.image(args[1:])
	default:
		engine.t.Fatalf("unexpected Docker command: %v", args)
		return nil, nil
	}
}

func (engine *fakeImageEngine) image(args []string) ([]byte, error) {
	switch args[0] {
	case "ls":
		return []byte(engine.tags[strings.TrimPrefix(imageArgument(args, "--filter"), "reference=")]), nil
	case "inspect":
		id := args[1]
		if alias := engine.tags[id]; alias != "" {
			id = alias
		}
		description, ok := engine.images[id]
		if !ok {
			return nil, fmt.Errorf("missing fake image %s", id)
		}
		return json.Marshal([]dockerImageDescription{description})
	case "tag":
		if engine.tags[args[2]] != "" {
			engine.t.Fatal("overwrote an existing tag")
		}
		engine.tags[args[2]] = args[1]
		return nil, nil
	case "rm":
		if !strings.HasPrefix(args[1], "goobers-release-input:") {
			engine.t.Fatalf("deleted a non-owned image tag: %v", args)
		}
		delete(engine.tags, args[1])
		return nil, nil
	default:
		engine.t.Fatalf("unexpected image command: %v", args)
		return nil, nil
	}
}

func (engine *fakeImageEngine) build(args []string) ([]byte, error) {
	reference := imageArgument(args, "--tag")
	if engine.tags[reference] != "" {
		engine.t.Fatal("build overwrote an existing tag")
	}
	family := "goobers-base"
	for _, candidate := range imageFamilies(engine.platform) {
		if strings.Contains(reference, "/"+candidate+":") {
			family = candidate
		}
	}
	id := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(reference)))
	description := dockerImageDescription{ID: id, OS: engine.platform.OS, Architecture: engine.platform.Arch}
	description.Config.User = "65532:65532"
	if engine.platform.OS == "windows" {
		description.Config.User = "ContainerUser"
	}
	directory := args[len(args)-1]
	if strings.Contains(family, "harness") {
		var alias string
		for _, arg := range args {
			if strings.HasPrefix(arg, "GOOBERS_BASE_IMAGE=") {
				alias = strings.TrimPrefix(arg, "GOOBERS_BASE_IMAGE=")
			}
		}
		base := engine.tags[alias]
		if base == "" {
			engine.t.Fatal("harness did not use a bound immutable base alias")
		}
		description.RootFS.Layers = append(append([]string(nil), engine.images[base].RootFS.Layers...), "harness-layer")
		if engine.wrongBase {
			description.RootFS.Layers[0] = "unrelated-base-layer"
		}
		engine.metadata[id], engine.digests[id] = engine.metadata[base], engine.digests[base]
	} else {
		description.RootFS.Layers = []string{"base-runtime-layer", "release-binary-layer"}
		data, err := os.ReadFile(filepath.Join(directory, "release.json"))
		if err != nil {
			engine.t.Fatal(err)
		}
		var metadata imageContextMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			engine.t.Fatal(err)
		}
		engine.metadata[id] = metadata
		engine.digests[id] = make(map[string]string)
		for _, binary := range []string{"goobers", "goobers-operator"} {
			name := binary
			if engine.platform.OS == "windows" {
				name += ".exe"
			}
			digest, err := sha256Hex(filepath.Join(directory, name))
			if err != nil {
				engine.t.Fatal(err)
			}
			engine.digests[id][binary] = digest
		}
	}
	engine.tags[reference], engine.images[id], engine.families[id] = id, description, family
	return nil, nil
}

func (engine *fakeImageEngine) probe(args []string) ([]byte, error) {
	entrypoint := imageArgument(args, "--entrypoint")
	var id string
	for _, arg := range args {
		if imageIDPattern.MatchString(arg) {
			id = arg
		}
	}
	if id == "" {
		engine.t.Fatal("runtime verification used a mutable tag")
	}
	if engine.platform.OS == "linux" {
		for _, flag := range []string{"--read-only", "no-new-privileges", "/tmp:noexec", "/home/nonroot:noexec,uid=65532,gid=65532"} {
			if !strings.Contains(strings.Join(args, " "), flag) {
				engine.t.Fatalf("missing restricted runtime flag %s", flag)
			}
		}
	}
	metadata, digests := engine.metadata[id], engine.digests[id]
	switch entrypoint {
	case "goobers", "goobers-operator":
		if engine.badStamp {
			metadata.Commit = "wrong"
		}
		return []byte(fmt.Sprintf("%s %s (commit %s, built %s, go1.26.6 %s)", entrypoint, metadata.Version, metadata.Commit, metadata.Date, metadata.Platform)), nil
	case "/bin/sh", "powershell":
		if strings.Contains(args[len(args)-1], "harness/version") {
			return []byte("1.2.3"), nil
		}
		goobersHash := digests["goobers"]
		if engine.families[id] == engine.badHashFamily {
			goobersHash = strings.Repeat("0", 64)
		}
		if entrypoint == "powershell" {
			return []byte("ContainerUser\n" + goobersHash + "\n" + digests["goobers-operator"]), nil
		}
		return []byte("65532\n65532\n" + goobersHash + "  /usr/local/bin/goobers\n" + digests["goobers-operator"] + "  /usr/local/bin/goobers-operator"), nil
	case "copilot":
		if imageArgument(args, "--usage-output-file") != "" {
			if imageArgument(args, "--network") != "none" || imageArgument(args, "--workdir") != "/tmp" {
				engine.t.Fatal("Copilot parser probe lacks offline writable scratch isolation")
			}
			for _, arg := range args {
				if arg == "--help" || arg == "--version" {
					engine.t.Fatal("Copilot parser probe bypasses real option parsing")
				}
			}
			output, code := engine.copilotProbeOutput, engine.copilotProbeCode
			if output == "" {
				output, code = "Error: No authentication information found.\n", 1
			}
			if code == 0 {
				return []byte(output), nil
			}
			return []byte(output), imageProbeExitError{code}
		}
		return []byte("GitHub Copilot CLI 1.2.3."), nil
	case "claude":
		return []byte("1.2.3 (Claude Code)"), nil
	default:
		return nil, fmt.Errorf("unexpected probe entrypoint %s", entrypoint)
	}
}

func TestReleaseBuildImagesRecordsAllLinuxFamilies(t *testing.T) {
	useImageTestBinaries(t)
	engine := useFakeImageEngine(t, Target{"linux", "arm64"})
	root := t.TempDir()
	var stdout strings.Builder
	err := run(append(imageTestArgs(root), "-targets", "linux/arm64", "-build-images", "-image-prefix", "local-test"), &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "built and verified local images") || strings.Contains(stdout.String(), "images not built") {
		t.Fatalf("incorrect image completion status: %s", stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "images", "image-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var evidence localImageEvidence
	if err := json.Unmarshal(data, &evidence); err != nil {
		t.Fatal(err)
	}
	if len(evidence.Images) != 3 || !evidence.Native || evidence.EnginePlatform != "linux/arm64" || len(evidence.HarnessSHA256) != 2 {
		t.Fatalf("incomplete image evidence: %+v", evidence)
	}
	if len(engine.tags) != 3 {
		t.Fatalf("temporary aliases remain: %v", engine.tags)
	}
	for index, image := range evidence.Images {
		if image.BinarySHA256["goobers"] == "" || image.BinarySHA256["goobers-operator"] == "" || image.VersionOutput["goobers"] == "" {
			t.Fatalf("unbound binary evidence: %+v", image)
		}
		if index > 0 && (image.BaseImageID != evidence.Images[0].ImageID || image.HarnessVersion == "") {
			t.Fatalf("unbound harness base: %+v", image)
		}
		if image.Family == "goobers-harness-copilot" && (image.AdapterProbe == nil || image.AdapterProbe.ExitCode != 1) {
			t.Fatalf("Copilot image lacks real parser evidence: %+v", image)
		}
	}
	assertNoImageStaging(t, root)
}

func TestReleaseBuildImagesFailureKeepsContextsHiddenAndRemovesAlias(t *testing.T) {
	useImageTestBinaries(t)
	engine := useFakeImageEngine(t, Target{"linux", "arm64"})
	engine.badHashFamily = "goobers-harness-claude"
	root := t.TempDir()
	err := run(append(imageTestArgs(root), "-targets", "linux/arm64", "-build-images", "-image-prefix", "local-test"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "bytes differ from the release input") || !strings.Contains(err.Error(), "local image tags may remain") {
		t.Fatalf("image mismatch error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "images")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed verification exposed contexts: %v", err)
	}
	for tag := range engine.tags {
		if strings.HasPrefix(tag, "goobers-release-input:") {
			t.Fatalf("owned alias remains: %s", tag)
		}
	}
	assertNoImageStaging(t, root)
}

func TestReleaseBuildImagesWindowsBuildsOnlyBase(t *testing.T) {
	useImageTestBinaries(t)
	useWindowsImageFixtures(t, false)
	engine := useFakeImageEngine(t, Target{"windows", "amd64"})
	root := t.TempDir()
	if err := run(append(imageTestArgs(root), "-targets", "windows/amd64", "-build-images", "-image-prefix", "local-test"), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(engine.tags) != 1 {
		t.Fatalf("unexpected Windows families: %v", engine.tags)
	}
	for _, args := range engine.commands {
		if args[0] == "run" && strings.Contains(strings.Join(args, " "), "--read-only") {
			t.Fatal("applied unsupported Linux restriction to Windows")
		}
	}
}

func TestImageBuildPreflightProtectsPlatformsAndExistingTags(t *testing.T) {
	for _, scenario := range []string{"other architecture", "existing tag"} {
		t.Run(scenario, func(t *testing.T) {
			engine := useFakeImageEngine(t, Target{"linux", "arm64"})
			opts := options{buildImages: true, imageContexts: "images", imagePrefix: "local-test", version: "v0.4.0-rc.1", commit: "abcdef0", targets: []Target{{"linux", "arm64"}}}
			if scenario == "other architecture" {
				opts.targets = []Target{{"linux", "amd64"}}
			} else {
				engine.tags[opts.imagePrefix+"/goobers-base:"+localImageTag(opts, opts.targets[0])] = "preexisting-user-image"
			}
			if _, err := prepareLocalImageBuild(opts); err == nil {
				t.Fatal("unsafe image build was allowed")
			}
			for _, args := range engine.commands {
				if args[0] == "build" || args[0] == "run" {
					t.Fatalf("preflight mutated images: %v", args)
				}
			}
		})
	}
}

func TestDefaultReleaseDoesNotContactDocker(t *testing.T) {
	engine := useFakeImageEngine(t, Target{"linux", "arm64"})
	plan, err := prepareLocalImageBuild(options{})
	if err != nil || plan != nil || len(engine.commands) != 0 {
		t.Fatalf("default release contacted Docker: plan=%v err=%v commands=%v", plan, err, engine.commands)
	}
}

func TestLocalImageOptionsRequireExplicitSafeInputs(t *testing.T) {
	for _, scenario := range []string{"missing contexts", "missing prefix", "tagged prefix", "uppercase prefix", "nonhex commit", "oversized tag", "prefix without build"} {
		t.Run(scenario, func(t *testing.T) {
			opts := options{buildImages: true, imageContexts: "images", imagePrefix: "local-test", version: "v0.4.0-rc.1", commit: "abcdef0", targets: []Target{{"linux", "arm64"}}}
			switch scenario {
			case "missing contexts":
				opts.imageContexts = ""
			case "missing prefix":
				opts.imagePrefix = ""
			case "tagged prefix":
				opts.imagePrefix = "local-test:latest"
			case "uppercase prefix":
				opts.imagePrefix = "Local-Test"
			case "nonhex commit":
				opts.commit = "none"
			case "oversized tag":
				opts.version = strings.Repeat("v", 128)
			case "prefix without build":
				opts.buildImages = false
			}
			if err := validateLocalImageOptions(opts); err == nil {
				t.Fatal("invalid image options accepted")
			}
		})
	}
}

func TestVerifyBuiltImageRefusesStampAndBaseDrift(t *testing.T) {
	engine := useFakeImageEngine(t, Target{"linux", "arm64"})
	engine.badStamp = true
	id := "sha256:" + strings.Repeat("a", 64)
	description := dockerImageDescription{ID: id, OS: "linux", Architecture: "arm64"}
	description.Config.User = "65532:65532"
	metadata := imageContextMetadata{1, "goobers-base-build-inputs", "v0.4.0-rc.1", "abcdef0", "2026-09-07T12:00:00Z", "linux/arm64"}
	engine.metadata[id] = metadata
	if _, err := verifyBuiltImage(description, "goobers-base", "local-test/base", t.TempDir(), metadata); err == nil || !strings.Contains(err.Error(), "mismatched release stamp") {
		t.Fatalf("stamp drift was accepted: %v", err)
	}
	base := description
	base.RootFS.Layers = []string{"real-base"}
	engine.images[id] = base
	engine.tags["alias"] = id
	description.RootFS.Layers = []string{"other-base", "harness"}
	if err := verifyHarnessBase(description, base, "alias"); err == nil {
		t.Fatal("different harness base accepted")
	}
}

func TestImageAliasCleanupPreservesReplacement(t *testing.T) {
	engine := useFakeImageEngine(t, Target{"linux", "arm64"})
	expected := "sha256:" + strings.Repeat("a", 64)
	replacement := "sha256:" + strings.Repeat("b", 64)
	alias := "goobers-release-input:owned"
	engine.tags[alias] = replacement
	engine.images[replacement] = dockerImageDescription{ID: replacement}
	if err := removeOwnedImageAlias(alias, expected); err == nil {
		t.Fatal("alias replacement was not detected")
	}
	if engine.tags[alias] != replacement {
		t.Fatal("cleanup deleted another process's replacement image tag")
	}
}

func TestCopilotImageProbeRequiresRealParserAuthenticationRefusal(t *testing.T) {
	for _, scenario := range []struct {
		name, output string
		code         int
		accepted     bool
	}{
		{"supported parser", "Error: No authentication information found.\nAuthenticate before running Copilot.\n", 1, true},
		{"old parser", "error: unknown option '--usage-output-file'", 1, false},
		{"help bypass", "Usage: copilot [options]\n--usage-output-file <path>", 0, false},
		{"unexpected success", "ok", 0, false},
		{"engine failure", "Error: No authentication information found.", 125, false},
		{"unrelated failure", "Error: failed to create config directory", 1, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			engine := useFakeImageEngine(t, Target{"linux", "arm64"})
			engine.copilotProbeOutput, engine.copilotProbeCode = scenario.output, scenario.code
			description := dockerImageDescription{ID: "sha256:" + strings.Repeat("a", 64), OS: "linux", Architecture: "arm64"}
			proof, err := verifyCopilotAdapterInterface(description)
			if (err == nil) != scenario.accepted {
				t.Fatalf("probe result=%+v error=%v", proof, err)
			}
			if proof != nil && (proof.ExitCode != 1 || proof.Output != strings.TrimSpace(scenario.output) || !strings.Contains(proof.Scope, "not verified")) {
				t.Fatalf("probe overstates its evidence: %+v", proof)
			}
			joined := strings.Join(engine.commands[len(engine.commands)-1], " ")
			for _, want := range []string{"COPILOT_GITHUB_TOKEN=", "GH_TOKEN=", "GITHUB_TOKEN=", "--available-tools=", "--usage-output-file /tmp/goobers-usage-probe.json"} {
				if !strings.Contains(joined, want) {
					t.Fatalf("missing probe contract %s: %s", want, joined)
				}
			}
		})
	}
}

func TestReleaseBuildImagesRejectsUnsupportedCopilotAdapterOption(t *testing.T) {
	useImageTestBinaries(t)
	engine := useFakeImageEngine(t, Target{"linux", "arm64"})
	engine.copilotProbeOutput = "error: unknown option '--usage-output-file'"
	engine.copilotProbeCode = 1
	root := t.TempDir()
	err := run(append(imageTestArgs(root), "-targets", "linux/arm64", "-build-images", "-image-prefix", "local-test"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unknown option '--usage-output-file'") {
		t.Fatalf("unsupported adapter interface passed release image verification: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "images")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incompatible image exposed completed contexts: %v", err)
	}
	for tag := range engine.tags {
		if strings.HasPrefix(tag, "goobers-release-input:") {
			t.Fatalf("failed parser gate left base alias %s", tag)
		}
	}
	assertNoImageStaging(t, root)
}
