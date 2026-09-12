package main

import (
	"fmt"
	"strings"
)

type imageStageSmokeEvidence struct {
	Scope      string                  `json:"scope"`
	Entrypoint string                  `json:"entrypoint"`
	Arguments  []string                `json:"arguments"`
	Output     string                  `json:"output"`
	DSL3       *imageDSL3SmokeEvidence `json:"dsl3,omitempty"`
}

type imageDSL3SmokeEvidence struct {
	Scope      string   `json:"scope"`
	Entrypoint string   `json:"entrypoint"`
	Arguments  []string `json:"arguments"`
	Output     string   `json:"output"`
}

const linuxImageStageSmoke = `goobers init --allow-ephemeral --template=quickstart /tmp/quickstart-instance
goobers validate /tmp/quickstart-instance
goobers init --allow-ephemeral --demo --insecure /tmp/demo-instance
GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE=1 goobers run demo /tmp/demo-instance
`

const windowsImageStageSmoke = `$ErrorActionPreference = 'Stop'
$root = Join-Path $env:TEMP 'goobers-image-smoke'
$quickstart = Join-Path $root 'quickstart-instance'
$demo = Join-Path $root 'demo-instance'
goobers init --allow-ephemeral --template=quickstart $quickstart
if ($LASTEXITCODE -ne 0) { throw 'Image quickstart initialization failed' }
goobers validate $quickstart
if ($LASTEXITCODE -ne 0) { throw 'Image quickstart validation failed' }
goobers init --allow-ephemeral --demo --insecure $demo
if ($LASTEXITCODE -ne 0) { throw 'Image mock demo initialization failed' }
$env:GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE = '1'
goobers run demo $demo
if ($LASTEXITCODE -ne 0) { throw 'Image mock demo execution failed' }
`

// The Manifest annotation is the supported, explicit preview opt-in. It is
// applied only to this fresh fixture; fix migrates the workflow mechanically.
const linuxImageDSL3Smoke = `goobers init --allow-ephemeral --demo --insecure /tmp/dsl3-demo
while IFS= read -r line; do
  printf '%s\n' "$line"
  if [ "$line" = 'metadata:' ]; then
    printf '  annotations:\n    goobers.dev/allow-preview-features: "true"\n'
  fi
done < /tmp/dsl3-demo/config/manifest.yaml > /tmp/dsl3-demo/config/manifest-preview.yaml
mv /tmp/dsl3-demo/config/manifest-preview.yaml /tmp/dsl3-demo/config/manifest.yaml
goobers fix --to 3.0 --write /tmp/dsl3-demo
goobers validate /tmp/dsl3-demo
GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE=1 goobers run demo /tmp/dsl3-demo
`

const windowsImageDSL3Smoke = `$ErrorActionPreference = 'Stop'
$demo = Join-Path $env:TEMP 'goobers-image-dsl3-smoke'
goobers init --allow-ephemeral --demo --insecure $demo
if ($LASTEXITCODE -ne 0) { throw 'Image DSL 3.0 demo initialization failed' }
$manifest = Join-Path $demo 'config\manifest.yaml'
$text = [IO.File]::ReadAllText($manifest)
$annotation = 'metadata:' + [Environment]::NewLine + '  annotations:' + [Environment]::NewLine + '    goobers.dev/allow-preview-features: "true"'
$text = [regex]::Replace($text, '(?m)^metadata:\r?$', $annotation)
[IO.File]::WriteAllText($manifest, $text, (New-Object Text.UTF8Encoding($false)))
goobers fix --to 3.0 --write $demo
if ($LASTEXITCODE -ne 0) { throw 'Image DSL 3.0 migration failed' }
goobers validate $demo
if ($LASTEXITCODE -ne 0) { throw 'Image DSL 3.0 validation failed' }
$env:GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE = '1'
goobers run demo $demo
if ($LASTEXITCODE -ne 0) { throw 'Image DSL 3.0 mock demo execution failed' }
`

// Run only the shipped, credential-free mock demo in a fresh network-disabled
// container. The documented unisolated-stage opt-in does not grant networking:
// the container boundary remains closed. No authenticated harness is invoked.
func verifyImageStageSmoke(description dockerImageDescription) (*imageStageSmokeEvidence, error) {
	entrypoint, command := "/bin/sh", []string{"-ec", linuxImageStageSmoke}
	if description.OS == "windows" {
		entrypoint, command = "powershell", []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", windowsImageStageSmoke}
	}
	output, err := runImageProbe(description, entrypoint, command...)
	if err != nil {
		return nil, err
	}
	completed := false
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "finished: phase=completed" {
			completed = true
		}
	}
	if !completed {
		return nil, fmt.Errorf("image %s mock demo did not report completed phase: %s", description.ID, output)
	}
	dsl3, err := verifyImageDSL3Smoke(description)
	if err != nil {
		return nil, err
	}
	return &imageStageSmokeEvidence{Scope: "packaged DSL 2.0 quickstart and trusted four-stage mock demo in a network-disabled container; no authenticated harness, native per-stage sandbox or Kubernetes proof", Entrypoint: entrypoint, Arguments: command, Output: output, DSL3: dsl3}, nil
}

func verifyImageDSL3Smoke(description dockerImageDescription) (*imageDSL3SmokeEvidence, error) {
	entrypoint, command := "/bin/sh", []string{"-ec", linuxImageDSL3Smoke}
	if description.OS == "windows" {
		entrypoint, command = "powershell", []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", windowsImageDSL3Smoke}
	}
	output, err := runImageProbe(description, entrypoint, command...)
	if err != nil {
		return nil, err
	}
	if err := validateImageDSL3SmokeOutput(output); err != nil {
		return nil, fmt.Errorf("image %s DSL 3.0 smoke failed: %w\n%s", description.ID, err, output)
	}
	return &imageDSL3SmokeEvidence{Scope: "packaged CLI migration from DSL 2.0 to explicitly opted-in DSL 3.0 preview, validation and four-stage trusted mock completion in a fresh network-disabled container; no authenticated harness, native per-stage sandbox, distributed placement or support promotion proof", Entrypoint: entrypoint, Arguments: command, Output: output}, nil
}

func validateImageDSL3SmokeOutput(output string) error {
	var migrated, preview, completed bool
	stages := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "FIX Workflow/demo (") && strings.HasSuffix(line, "): migrated to dslVersion 3.0 (written)") {
			migrated = true
		}
		if line == "DSLVERSION Workflow/demo: 3.0 (preview)" {
			preview = true
		}
		if line == "finished: phase=completed" {
			completed = true
		}
		for _, stage := range []string{"curate", "implement", "review", "merge-preview"} {
			if strings.HasPrefix(line, "stage "+stage+" finished (run=") && strings.Contains(line, ", status=success,") {
				stages[stage] = true
			}
		}
	}
	if !migrated || !preview || !completed || len(stages) != 4 {
		return fmt.Errorf("require written migration, DSL 3.0 preview validation, completed phase and success for all four demo stages")
	}
	return nil
}
