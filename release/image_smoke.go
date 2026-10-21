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

// linuxImageStagePrepare is the container-free half: scaffolding and
// validation. TestShippedImageProbesRunWithRealBinary executes it verbatim in
// ordinary CI, so a product change that invalidates the probe is caught before
// a tag is signed rather than after — v0.4.0-rc.5 was signed and notarized
// before its probe turned out not to validate.
//
// GOOBERS_STAGE_SMOKE_DIR moves the target directory for that test; the image
// always uses the default paths.
const linuxImageStagePrepare = `root="${GOOBERS_STAGE_SMOKE_DIR:-/tmp}"
goobers init --allow-ephemeral --template=quickstart "$root/quickstart-instance"
goobers validate "$root/quickstart-instance"
goobers init --allow-ephemeral --demo --insecure "$root/demo-instance"
`

// The run stays image-only: a hermetic stage re-invokes goobers through a
// sanitized environment that does not inherit a test's temporary PATH, so only
// an image with goobers on the system path can execute it.
const linuxImageStageSmoke = linuxImageStagePrepare + `GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE=1 goobers run demo "$root/demo-instance"
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

// Exercise stable DSL 3.0 without a preview acknowledgement. Keep the
// preparation separately executable so ordinary CI checks the shipped probe.
const linuxImageDSL3Prepare = `demo="${GOOBERS_DSL3_SMOKE_DIR:-/tmp/dsl3-demo}"
goobers init --allow-ephemeral --demo --insecure "$demo"
goobers fix --to 3.0 --write "$demo"
goobers validate "$demo"
`

const linuxImageDSL3Smoke = linuxImageDSL3Prepare + `GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE=1 goobers run demo "$demo"
`

const windowsImageDSL3Smoke = `$ErrorActionPreference = 'Stop'
$demo = Join-Path $env:TEMP 'goobers-image-dsl3-smoke'
goobers init --allow-ephemeral --demo --insecure $demo
if ($LASTEXITCODE -ne 0) { throw 'Image DSL 3.0 demo initialization failed' }
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
	return &imageDSL3SmokeEvidence{Scope: "packaged CLI migration from DSL 2.0 to supported DSL 3.0, validation and four-stage trusted mock completion in a fresh network-disabled container; no authenticated harness, native per-stage sandbox, distributed placement or support promotion proof", Entrypoint: entrypoint, Arguments: command, Output: output}, nil
}

func validateImageDSL3SmokeOutput(output string) error {
	var migrated, preview, completed bool
	stages := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "FIX Workflow/demo (") && strings.HasSuffix(line, "): migrated to dslVersion 3.0 (written)") {
			migrated = true
		}
		if line == "DSLVERSION Workflow/demo: 3.0 (supported)" {
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
		return fmt.Errorf("require written migration, DSL 3.0 supported validation, completed phase and success for all four demo stages")
	}
	return nil
}
