package harness

import (
	"fmt"
	"path/filepath"

	"github.com/goobers/goobers/internal/mcpio"
)

type goobersIOMCPRuntime struct {
	ConfigPath string
	Command    string
	Args       []string
	Tools      []string
}

func prepareGoobersIOMCPRuntime(req RunRequest, selfBin string) (goobersIOMCPRuntime, bool, error) {
	if selfBin == "" || !autoGoobersIOEligible(req) {
		return goobersIOMCPRuntime{}, false, nil
	}

	artifactFile, _ := req.Envelope.Inputs[InputArtifactFile].(string)
	artifactManifestFile, _ := req.Envelope.Inputs[InputArtifactManifestFile].(string)
	cfg := mcpio.Config{
		Workspace:            req.Workspace,
		ChildWorkflows:       req.ChildWorkflows,
		ArtifactFile:         artifactFile,
		ArtifactManifestFile: artifactManifestFile,
		PublicationSchemas:   req.PublicationSchemas,
		ReceiptFile:          goobersIOReceiptFile(),
		Inputs:               req.ContextPaths,
		RunID:                req.Envelope.RunID,
		WorkflowID:           req.Envelope.WorkflowID,
		TaskID:               req.Envelope.TaskID,
		Gaggle:               req.Envelope.Gaggle,
	}
	if len(req.PublicationSchemas) > 0 {
		// The executor resets and journals this log around the invocation.
		cfg.PublicationReceiptFile = goobersIOPublicationReceiptFile()
	}
	configRel := filepath.Join(filepath.FromSlash(goobersIORuntimeSubdir), mcpio.ConfigFileName)
	configPath, err := mcpio.WriteConfig(req.Workspace, configRel, cfg)
	if err != nil {
		return goobersIOMCPRuntime{}, true, fmt.Errorf("write goobers-io config: %w", err)
	}
	if err := mcpio.ResetInputInspectionReceipts(req.Workspace, cfg.ReceiptFile); err != nil {
		return goobersIOMCPRuntime{}, true, fmt.Errorf("reset goobers-io input inspection receipts: %w", err)
	}

	return goobersIOMCPRuntime{
		ConfigPath: configPath,
		Command:    selfBin,
		Args:       []string{"mcp-io", "--config", configPath},
		Tools:      goobersIOToolsFor(req),
	}, true, nil
}
