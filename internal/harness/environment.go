package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/telemetry"
)

type credentialEnvConfig struct {
	adapterName                    string
	envCapabilities                map[string]string
	optionalCredentialCapabilities map[string]bool
	extraEnvAllowlist              []string
	instanceRoot                   string
	selfBin                        string
}

func baseEnv(extra []string) []string {
	return procenv.BaseEnvWith(extra)
}

func buildCredentialEnv(ctx context.Context, cfg credentialEnvConfig, req RunRequest) ([]string, error) {
	env := baseEnv(cfg.extraEnvAllowlist)
	telemetryDir := req.TelemetryDir
	if telemetryDir == "" {
		telemetryDir = telemetry.PrepareStageTelemetryDir(req.Workspace)
	}
	if telemetryDir != "" {
		env = append(env, telemetry.StageTelemetryEnv+"="+telemetryDir)
	}
	if cfg.instanceRoot != "" {
		env = append(env, executor.InstanceRootEnvVar+"="+cfg.instanceRoot)
	}
	if cfg.selfBin != "" {
		env = append(env, executor.GoobersBinEnvVar+"="+cfg.selfBin)
	}
	if repo := req.Envelope.RepoRef; repo.Provider != "" {
		env = append(env,
			executor.RepoProviderEnvVar+"="+string(repo.Provider),
			executor.RepoOwnerEnvVar+"="+repo.Owner,
			executor.RepoNameEnvVar+"="+repo.Name,
		)
	}
	if refs := req.Envelope.AdditionalWorkspaces; len(refs) > 0 {
		type refWorkspace struct{ name, path string }
		items := make([]refWorkspace, 0, len(refs))
		for _, workspace := range refs {
			if workspace.Name == "" || workspace.Path == "" {
				continue
			}
			items = append(items, refWorkspace{workspace.Name, workspace.Path})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
		names := make([]string, 0, len(items))
		for _, item := range items {
			env = append(env, executor.AdditionalRepoEnvVar(item.name)+"="+item.path)
			names = append(names, item.name)
		}
		if len(names) > 0 {
			env = append(env, executor.AdditionalReposEnvVar+"="+strings.Join(names, ","))
		}
	}
	for _, capability := range req.Envelope.Capabilities {
		envVar, ok := cfg.envCapabilities[capability]
		if !ok {
			continue
		}
		if req.Credentials == nil {
			return nil, fmt.Errorf("harness: %s: resolve %s: no credential set", cfg.adapterName, capability)
		}
		token, err := req.Credentials.Token(ctx, capability)
		if err != nil {
			if cfg.optionalCredentialCapabilities[capability] &&
				errors.Is(err, credentials.ErrNoCredentialForCapability) {
				continue
			}
			return nil, fmt.Errorf("harness: %s: resolve %s: %w", cfg.adapterName, capability, err)
		}
		env = append(env, envVar+"="+token)
	}
	return env, nil
}
