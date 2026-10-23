// Command chaos runs the HA chaos scenarios and writes the evidence bundle.
//
//	chaos list   print scenarios and their pending state
//	chaos run    run scenarios; pending ones are reported as skipped
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/test/ha/chaos"
	"github.com/goobers/goobers/test/ha/invariants"
)

func main() {
	if len(os.Args) != 2 {
		exitf("usage: chaos list|run|verify-pending|freeze-smoke")
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
	case "freeze-smoke":
		if err := freezeSmoke(); err != nil {
			exitf("%v", err)
		}
	case "verify-pending":
		if err := verifyPending(); err != nil {
			exitf("%v", err)
		}
	case "run":
		if err := run(); err != nil {
			exitf("%v", err)
		}
	default:
		exitf("unknown command %q", os.Args[1])
	}
}

// freezeSmoke runs CgroupFreeze against the current kubectl context and writes
// the evidence lines to HA_EVIDENCE_DIR/freeze-smoke.txt and stdout.
func freezeSmoke() error {
	evidence := os.Getenv("HA_EVIDENCE_DIR")
	if evidence == "" {
		return fmt.Errorf("HA_EVIDENCE_DIR is required")
	}
	lines, err := chaos.FreezeSmoke(context.Background(), chaos.OSExecutor{}, time.Sleep)
	text := strings.Join(lines, "\n") + "\n"
	fmt.Print(text)
	if werr := os.MkdirAll(evidence, 0o755); werr == nil {
		_ = os.WriteFile(filepath.Join(evidence, "freeze-smoke.txt"), []byte(text), 0o644)
	}
	return err
}

// verifyPending fails when a pending reference is no longer open.
func verifyPending() error {
	seen := map[string]bool{}
	for _, s := range chaos.Scenarios() {
		if n, ok := chaos.PendingNumber(s.Pending); ok {
			seen[n] = true
		}
	}
	for _, inv := range invariants.All() {
		if n, ok := chaos.PendingNumber(inv.Pending); ok {
			seen[n] = true
		}
	}
	refs := make([]string, 0, len(seen))
	for n := range seen {
		refs = append(refs, n)
	}
	return chaos.VerifyPendingOpen(context.Background(), chaos.OSExecutor{}, refs)
}

// checkerFromEnv returns the invariant checker for runnable scenarios. It is
// required, not optional: with runnable scenarios and no database the run
// fails rather than passing unchecked.
func checkerFromEnv(ctx context.Context, evidence string) (func(context.Context) (int, error), func(), error) {
	url := os.Getenv("HA_INVARIANT_DB_URL")
	if url == "" {
		return nil, nil, fmt.Errorf("runnable scenarios need HA_INVARIANT_DB_URL for the invariant checker")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	check := func(ctx context.Context) (int, error) {
		report, err := invariants.Check(ctx, pool)
		if err != nil {
			return 0, err
		}
		if err := report.WriteEvidence(evidence); err != nil {
			return 0, err
		}
		return len(report.Violations) + len(report.Uncovered), nil
	}
	return check, pool.Close, nil
}

func run() error {
	evidence := os.Getenv("HA_EVIDENCE_DIR")
	if evidence == "" {
		return fmt.Errorf("HA_EVIDENCE_DIR is required")
	}
	env := chaos.Env{Exec: chaos.OSExecutor{}}
	ctx := context.Background()
	// An aborted earlier run must not leave a frozen pod: sweep before and after.
	if err := chaos.Sweep(ctx, env.Exec, chaos.DefaultChaosNamespace); err != nil {
		return fmt.Errorf("sweep at start: %w", err)
	}
	for _, s := range chaos.Scenarios() {
		if s.Pending == "" {
			check, closeDB, err := checkerFromEnv(context.Background(), evidence)
			if err != nil {
				return err
			}
			defer closeDB()
			env.Check = check
			break
		}
	}
	summary, err := chaos.RunAll(ctx, chaos.Scenarios(), env)
	// The sweep must run even if a scenario aborted the run.
	if sweepErr := chaos.Sweep(context.WithoutCancel(ctx), env.Exec, chaos.DefaultChaosNamespace); sweepErr != nil {
		return errors.Join(err, fmt.Errorf("sweep at end: %w", sweepErr))
	}
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
