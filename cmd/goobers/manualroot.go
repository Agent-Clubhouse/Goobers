package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/goobers/goobers/internal/runtimewiring"

	"github.com/goobers/goobers/internal/instance"
)

// Remote commands must obtain identity from their remote daemon, never label
// that mutation with the unrelated local default directory.
func prepareLocalManualRoot(layout instance.Layout, endpoint string, diagnostic io.Writer) error {
	if endpoint != "" {
		return prepareRemoteRoot(context.Background(), endpoint, diagnostic)
	}
	return runtimewiring.PrepareManualRoot(layout, diagnostic)
}

func prepareMaintenanceRoot(layout instance.Layout, dryRun bool, diagnostic io.Writer) error {
	if !dryRun {
		return runtimewiring.PrepareManualRoot(layout, diagnostic)
	}
	// A preview must neither adopt a legacy root nor reject historical data
	// merely because an actual mutation would be refused.
	return displayRootInspection(layout, diagnostic)
}

// Security remediation must remain possible on historical or damaged roots.
// Its banner reports uncertainty without adopting or inventing an identity.
func displayRootInspection(layout instance.Layout, diagnostic io.Writer) error {
	_, err := fmt.Fprint(diagnostic, statusRootText(layout, time.Now()))
	return err
}
