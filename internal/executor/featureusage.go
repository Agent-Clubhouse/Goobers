package executor

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/diagnostics/featureusage"
	"github.com/goobers/goobers/internal/telemetry"
)

func (e *ShellExecutor) prepareFeatureUsage(env apiv1.InvocationEnvelope, stageEnv []string) ([]string, func()) {
	dir := telemetry.PrepareStageTelemetryDir(env.Workspace)
	featureusage.BeginProviderWindow(dir)
	if dir != "" {
		stageEnv = append(stageEnv, telemetry.StageTelemetryEnv+"="+dir)
	}
	return stageEnv, func() { featureusage.CollectProviderWindow(dir, e.Journal, env.TaskID) }
}
