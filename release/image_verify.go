package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func verifyBuiltImage(description dockerImageDescription, family, reference, directory string, expected imageContextMetadata) (builtImageEvidence, error) {
	evidence := builtImageEvidence{
		Family: family, Reference: reference, ImageID: description.ID,
		Platform: description.OS + "/" + description.Architecture, User: description.Config.User,
		BinarySHA256: make(map[string]string), VersionOutput: make(map[string]string),
	}
	if evidence.Platform != expected.Platform {
		return evidence, fmt.Errorf("image %s platform %s differs from release %s", reference, evidence.Platform, expected.Platform)
	}
	user := "65532:65532"
	if description.OS == "windows" {
		user = "ContainerUser"
	}
	if !strings.EqualFold(description.Config.User, user) {
		return evidence, fmt.Errorf("image %s user %q does not satisfy %s", reference, description.Config.User, user)
	}
	for _, binary := range []string{"goobers", "goobers-operator"} {
		output, err := runImageProbe(description, binary, "--version")
		if err != nil {
			return evidence, err
		}
		stamp := expected.Version + " (commit " + expected.Commit + ", built " + expected.Date + ", "
		if !strings.Contains(output, stamp) || !strings.HasSuffix(output, " "+expected.Platform+")") {
			return evidence, fmt.Errorf("image %s %s has a mismatched release stamp: %s", reference, binary, output)
		}
		evidence.VersionOutput[binary] = output
	}
	digests, err := probeImageBinaryDigests(description)
	if err != nil {
		return evidence, err
	}
	for _, binary := range []string{"goobers", "goobers-operator"} {
		name := binary
		if description.OS == "windows" {
			name += ".exe"
		}
		expectedDigest, err := sha256Hex(filepath.Join(directory, name))
		if err != nil {
			return evidence, err
		}
		if digests[binary] != expectedDigest {
			return evidence, fmt.Errorf("image %s %s bytes differ from the release input", reference, binary)
		}
		evidence.BinarySHA256[binary] = expectedDigest
	}
	if family == "goobers-harness-copilot" {
		proof, err := verifyCopilotAdapterInterface(description)
		if err != nil {
			return evidence, err
		}
		evidence.AdapterProbe = proof
	}
	return evidence, nil
}

func runImageProbe(description dockerImageDescription, entrypoint string, command ...string) (string, error) {
	args := imageProbeRunArgs(description)
	args = append(args, "--entrypoint", entrypoint, description.ID)
	args = append(args, command...)
	output, err := dockerImageCommand(2*time.Minute, args...)
	if err != nil {
		return "", fmt.Errorf("verify image %s with %s: %w\n%s", description.ID, entrypoint, err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

func imageProbeRunArgs(description dockerImageDescription) []string {
	args := []string{"run", "--rm", "--pull", "never", "--platform", description.OS + "/" + description.Architecture, "--network", "none"}
	if description.OS == "linux" {
		args = append(args, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
			"--tmpfs", "/tmp:noexec", "--tmpfs", "/home/nonroot:noexec,uid=65532,gid=65532")
	}
	return args
}

type copilotAdapterProbeEvidence struct {
	Scope     string   `json:"scope"`
	Arguments []string `json:"arguments"`
	ExitCode  int      `json:"exitCode"`
	Output    string   `json:"output"`
}

// A --version check cannot detect an incompatible argument parser, and --help
// bypasses unknown-option validation in Copilot. Exercise the real prompt path
// without credentials or networking and require its specific auth refusal.
func verifyCopilotAdapterInterface(description dockerImageDescription) (*copilotAdapterProbeEvidence, error) {
	command := []string{"-p", "Reply with ok.", "--allow-all-tools", "--available-tools=", "--log-level", "all", "--usage-output-file", "/tmp/goobers-usage-probe.json"}
	args := imageProbeRunArgs(description)
	args = append(args, "--env", "COPILOT_GITHUB_TOKEN=", "--env", "GH_TOKEN=", "--env", "GITHUB_TOKEN=", "--workdir", "/tmp",
		"--entrypoint", "copilot", description.ID)
	args = append(args, command...)
	output, err := dockerImageCommand(2*time.Minute, args...)
	message := strings.TrimSpace(string(output))
	if err == nil {
		return nil, fmt.Errorf("image %s Copilot adapter parser probe unexpectedly succeeded without authentication: %s", description.ID, message)
	}
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.HasPrefix(message, "Error: No authentication information found.") {
		return nil, fmt.Errorf("image %s Copilot adapter parser probe did not reach the expected missing-auth refusal: %w\n%s", description.ID, err, message)
	}
	return &copilotAdapterProbeEvidence{
		Scope:     "adapter argument parsing reached missing-auth refusal; authenticated execution and usage-file production are not verified",
		Arguments: command, ExitCode: exit.ExitCode(), Output: message,
	}, nil
}

func probeImageBinaryDigests(description dockerImageDescription) (map[string]string, error) {
	if description.OS == "windows" {
		return probeWindowsImageBinaryDigests(description)
	}
	output, err := runImageProbe(description, "/bin/sh", "-ec", "id -u; id -g; sha256sum /usr/local/bin/goobers /usr/local/bin/goobers-operator")
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(output)
	if len(fields) != 6 || fields[0] != "65532" || fields[1] != "65532" || fields[3] != "/usr/local/bin/goobers" || fields[5] != "/usr/local/bin/goobers-operator" {
		return nil, fmt.Errorf("image %s returned invalid non-root binary checksum evidence", description.ID)
	}
	return map[string]string{"goobers": fields[2], "goobers-operator": fields[4]}, nil
}

func probeWindowsImageBinaryDigests(description dockerImageDescription) (map[string]string, error) {
	script := "$ErrorActionPreference = 'Stop'; $env:USERNAME; " +
		"(Get-FileHash -Algorithm SHA256 -LiteralPath C:\\Goobers\\goobers.exe).Hash.ToLowerInvariant(); " +
		"(Get-FileHash -Algorithm SHA256 -LiteralPath C:\\Goobers\\goobers-operator.exe).Hash.ToLowerInvariant()"
	output, err := runImageProbe(description, "powershell", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(output)
	if len(fields) != 3 || !strings.EqualFold(fields[0], "ContainerUser") {
		return nil, fmt.Errorf("image %s returned invalid ContainerUser binary checksum evidence", description.ID)
	}
	return map[string]string{"goobers": fields[1], "goobers-operator": fields[2]}, nil
}

func verifyImageHarness(description dockerImageDescription, harness string) (string, error) {
	expected, err := runImageProbe(description, "/bin/sh", "-ec", "cat /usr/share/goobers/harness/version")
	if err != nil {
		return "", err
	}
	actual, err := runImageProbe(description, harness, "--version")
	if err != nil {
		return "", err
	}
	want := expected + " (Claude Code)"
	first, _, _ := strings.Cut(actual, "\n")
	if harness == "copilot" {
		want = "GitHub Copilot CLI " + expected + "."
	}
	if expected == "" || first != want {
		return "", fmt.Errorf("image %s %s runtime version differs from its pinned input: %s", description.ID, harness, actual)
	}
	return actual, nil
}
