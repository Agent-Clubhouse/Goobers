// Command chaos runs the HA chaos scenarios and writes the evidence bundle.
//
//	chaos list   print scenarios and their pending state
//	chaos run    run scenarios; pending ones are reported as skipped
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/goobers/goobers/test/ha/chaos"
)

func main() {
	if len(os.Args) != 2 {
		exitf("usage: chaos list|run")
	}
	switch os.Args[1] {
	case "list":
		for _, s := range chaos.Scenarios() {
			state := "runnable"
			if s.Pending != "" {
				state = s.Pending
			}
			fmt.Printf("%s\t%s\n", s.ID, state)
		}
	case "run":
		if err := run(); err != nil {
			exitf("%v", err)
		}
	default:
		exitf("unknown command %q", os.Args[1])
	}
}

func run() error {
	evidence := os.Getenv("HA_EVIDENCE_DIR")
	if evidence == "" {
		return fmt.Errorf("HA_EVIDENCE_DIR is required")
	}
	env := chaos.Env{Exec: chaos.OSExecutor{}}
	summary, err := chaos.RunAll(context.Background(), chaos.Scenarios(), env)
	if err != nil {
		return err
	}
	if err := summary.WriteEvidence(evidence); err != nil {
		return err
	}
	passed, failed, skipped := summary.Counts()
	fmt.Printf("chaos: %d passed, %d failed, %d skipped (pending)\n", passed, failed, skipped)
	for _, r := range summary.Results {
		fmt.Printf("  %s\t%s\t%s\n", r.ID, r.Status, r.Reason)
	}
	if failed > 0 {
		return fmt.Errorf("%d scenario(s) failed", failed)
	}
	return nil
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
