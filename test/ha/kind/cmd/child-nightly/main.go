// child-nightly gates and runs real Kubernetes child-family qualification.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/goobers/goobers/test/ha/chaos"
	"github.com/goobers/goobers/test/ha/kind/child"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 1 {
		return fmt.Errorf("usage: child-nightly")
	}
	evidence := os.Getenv("HA_EVIDENCE_DIR")
	if evidence == "" {
		return fmt.Errorf("HA_EVIDENCE_DIR is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	address := os.Getenv("CHILD_TEMPORAL_ADDRESS")
	if address == "" {
		return fmt.Errorf("CHILD_TEMPORAL_ADDRESS is required")
	}
	return child.Run(ctx, chaos.OSExecutor{}, address, evidence)
}
