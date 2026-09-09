package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/mutationsidecar"
)

// A remote CLI stage already receives a run-contained journal bearer, never
// the pod's surrender token. Acknowledge intents before contacting the forge
// and confirmations before returning, so destroying an ephemeral pod cannot
// erase an acknowledged landing attempt. Local stages retain the fsynced
// sidecar and the host's existing cleanup recovery path.
func publishStageLandingReceipts(ctx context.Context) error {
	endpoint, token := os.Getenv(journalclient.EnvEndpoint), os.Getenv(journalclient.EnvToken)
	if endpoint == "" && token == "" {
		return nil
	}
	runID, gaggle := os.Getenv(journalclient.EnvRunID), os.Getenv(journalclient.EnvGaggle)
	if endpoint == "" || token == "" || runID == "" || gaggle == "" {
		return fmt.Errorf("landing receipt handoff requires the complete run-scoped journal plane")
	}
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte(token))
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GOOBERS_CRED_") && value != "" {
			registry.Register([]byte(value))
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	emitter := &livejournal.HTTPEmitter{BaseURL: endpoint, Token: token, RetryDeadline: 3 * time.Second}
	return mutationsidecar.PublishBeforeCleanup(bounded, ".", "stage-landing-receipts", runID, gaggle, scrubber, emitter)
}
