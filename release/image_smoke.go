package main

import (
	"fmt"
	"strings"
)

type imageStageSmokeEvidence struct {
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
	return &imageStageSmokeEvidence{Scope: "packaged quickstart and trusted four-stage mock demo in a network-disabled container; no authenticated harness, native per-stage sandbox or Kubernetes proof", Entrypoint: entrypoint, Arguments: command, Output: output}, nil
}
