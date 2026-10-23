package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/providerconfig"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/providers"
)

// stageGaggleBacklog resolves the backlog configuration of the stage's gaggle
// (GOOBERS_GAGGLE) as a config set, and the gaggle's name. A stage that names
// no gaggle has nothing to apply and gets ("", nil, nil).
//
// The dispatcher's stamp wins over any config this process could read, as the
// bot-login stamp does (stageProviderConfiguredLogin): in a pod the only config
// reachable is one inside the workspace, content the run itself may have
// authored. The local executor never stamps it, so a local stage reads the
// instance config exactly as before.
//
// It FAILS CLOSED (#5957) whenever the settings cannot be resolved: an empty
// or malformed stamp, or no stamp and no readable instance config. Falling back
// to the defaults instead addressed the code repository's project and counted
// a Resolved predecessor as done against a stricter configured doneStates.
func stageGaggleBacklog(root string) (string, *instance.ConfigSet, error) {
	gaggle := os.Getenv(executor.GaggleEnvVar)
	if gaggle == "" {
		return "", nil, nil
	}
	if stamped, ok := os.LookupEnv(dispatcher.GaggleBacklogEnv); ok {
		settings, err := decodeStageBacklogSettings(gaggle, stamped)
		if err != nil {
			return gaggle, nil, err
		}
		return gaggle, settings.ConfigSet(gaggle), nil
	}
	set, report, err := instance.LoadConfigDir(layoutFor(root).ConfigDir())
	if err == nil && report != nil && set != nil {
		return gaggle, set, nil
	}
	reason := "no instance config here"
	if err != nil {
		reason = err.Error()
	}
	return gaggle, nil, fmt.Errorf("gaggle %q backlog settings cannot be resolved: the dispatcher stamped no %s and the instance config cannot be read (%s); refusing to fall back to the code repository's project and the default doneStates (#5957)",
		gaggle, dispatcher.GaggleBacklogEnv, reason)
}

func decodeStageBacklogSettings(gaggle, stamped string) (runtimewiring.StageBacklogSettings, error) {
	if strings.TrimSpace(stamped) == "" {
		return runtimewiring.StageBacklogSettings{}, fmt.Errorf("gaggle %q backlog settings cannot be resolved: the dispatcher stamped an empty %s, so it has no settings for this gaggle (#5957)", gaggle, dispatcher.GaggleBacklogEnv)
	}
	var settings runtimewiring.StageBacklogSettings
	if err := json.Unmarshal([]byte(stamped), &settings); err != nil {
		return runtimewiring.StageBacklogSettings{}, fmt.Errorf("gaggle %q backlog settings cannot be resolved: %s is malformed: %w", gaggle, dispatcher.GaggleBacklogEnv, err)
	}
	return settings, nil
}

// stageBacklogRepoRef is backlogRepoRefForStage with its failure: the
// RepositoryRef an Azure DevOps stage's work item operations address, or the
// reason the gaggle's backlog project cannot be applied.
func stageBacklogRepoRef(root string, routed providers.RepositoryRef) (providers.RepositoryRef, error) {
	if routed.Provider != providers.ProviderADO {
		return routed, nil
	}
	gaggle, set, err := stageGaggleBacklog(root)
	if err != nil || gaggle == "" {
		return routed, err
	}
	ref, err := providerconfig.ApplyBacklogProject(set, gaggle, routed)
	if err != nil {
		return routed, fmt.Errorf("gaggle %q backlog project cannot be applied: %w", gaggle, err)
	}
	return ref, nil
}

// requireStageGaggleBacklog is the fail-closed gate every Azure DevOps stage
// provider passes (newRegisteredADOProviderForStage): a stage of a named
// gaggle whose backlog settings cannot be applied stops before it touches the
// backlog, rather than running against the code repository's project and the
// default doneStates (#5957).
func requireStageGaggleBacklog(root string, routed providers.RepositoryRef) error {
	_, err := stageBacklogRepoRef(root, routed)
	return err
}
