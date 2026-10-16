package main

import (
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/apireadcache"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"
)

func automationProviderReadScope(gaggle, generation string) apireadcache.Scope {
	return apireadcache.Scope{Gaggle: gaggle, Binding: "automation", Generation: generation}
}

func openPRReadIdentity(scope apireadcache.Scope, repo string) string {
	return fmt.Sprintf("%q/%q/%q/%q", scope.Gaggle, scope.Binding, scope.Generation, repo)
}

func configureStageADOReadCache(root string, provider *providers.ADOProvider) {
	provider.Client = apireadcache.ScopedClient(layoutFor(root).SchedulerDir(), os.Getenv(providersnapshot.EnvVar), stageReadScope(), providers.ProviderADO, provider.Client)
}

// Only ordinary runtime construction opts into the automation read partition.
// Human and isolated factories using the generic builder keep its zero value.
func buildAutomationDeterministicExecutor(input deterministicExecutorInput) (invoke.Deterministic, error) {
	input.AutomationReadCache = true
	return buildDeterministicExecutor(input)
}
