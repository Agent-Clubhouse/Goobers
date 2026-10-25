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
	if len(os.Args) != 2 || (os.Args[1] != "gate" && os.Args[1] != "run") {
		return fmt.Errorf("usage: child-nightly gate|run")
	}
	evidence := os.Getenv("HA_EVIDENCE_DIR")
	if evidence == "" {
		return fmt.Errorf("HA_EVIDENCE_DIR is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if os.Args[1] == "run" {
		if child.Pending != "" {
			return fmt.Errorf("child qualification is %s: requires production handoff adapter and config enablement", child.Pending)
		}
		address := os.Getenv("CHILD_TEMPORAL_ADDRESS")
		if address == "" {
			return fmt.Errorf("CHILD_TEMPORAL_ADDRESS is required")
		}
		return child.Run(ctx, chaos.OSExecutor{}, address, evidence)
	}
	ready := child.Pending == ""
	if !ready {
		ref, ok := chaos.PendingNumber(child.Pending)
		if !ok {
			return fmt.Errorf("invalid child pending marker %q", child.Pending)
		}
		if err := chaos.VerifyPendingOpen(ctx, chaos.OSExecutor{}, []string{ref}); err != nil {
			return err
		}
		if err := child.WritePending(evidence); err != nil {
			return err
		}
		fmt.Printf("::notice title=Child qualification pending::%s: requires production handoff adapter and instance config enablement; CHILD-HAPPY and CHAOS-13-CHILD remain unqualified\n", child.Pending)
	}
	output := os.Getenv("GITHUB_OUTPUT")
	if output == "" {
		return fmt.Errorf("GITHUB_OUTPUT is required")
	}
	file, err := os.OpenFile(output, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(file, "ready=%t\n", ready)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
