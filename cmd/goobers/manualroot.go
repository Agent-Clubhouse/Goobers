package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// prepareManualRoot identifies the actual resolved target before command
// mutations. Identity adoption is the sole bootstrap write; normal inspection
// never calls this helper. Use stderr so JSON command output stays machine-readable.
func prepareManualRoot(layout instance.Layout, diagnostic io.Writer) error {
	if err := validateStartupRoot(layout); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := instance.EnsureRootIdentity(ctx, layout.Root)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(diagnostic, "Instance root: %q; instance ID: %q\n", canonicalStatusRoot(layout.Root), id); err != nil {
		return fmt.Errorf("display mutation target: %w", err)
	}
	return nil
}

// Remote commands must obtain identity from their remote daemon, never label
// that mutation with the unrelated local default directory.
func prepareLocalManualRoot(layout instance.Layout, endpoint string, diagnostic io.Writer) error {
	if endpoint != "" {
		return prepareRemoteRoot(context.Background(), endpoint, diagnostic)
	}
	return prepareManualRoot(layout, diagnostic)
}

func prepareMaintenanceRoot(layout instance.Layout, dryRun bool, diagnostic io.Writer) error {
	if !dryRun {
		return prepareManualRoot(layout, diagnostic)
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
