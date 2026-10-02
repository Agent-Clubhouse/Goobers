package main

import (
	"fmt"
	"io"

	"github.com/goobers/goobers/internal/instance"
)

// Restore an interrupted generation before validating the live root: a crash
// can leave instance.yaml absent until the durable decision is replayed.
func prepareDaemonStartupRoot(layout instance.Layout, stderr io.Writer) error {
	if err := instance.RecoverConfigTransaction(layout); err != nil {
		return fmt.Errorf("recover configuration: %w", err)
	}
	return prepareManualRoot(layout, stderr)
}
