package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/goobers/goobers/internal/readservice"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(runMain(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func runMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "fixture" {
		return runFixture(ctx, args[1:], stderr)
	}
	fs := flag.NewFlagSet("soak", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("profile", "standard", "versioned preset: smoke, standard, hostile")
	binary := fs.String("goobers", "goobers", "real CLI binary")
	root := fs.String("root", "", "new scratch instance path inside isolated runtime (retained)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p, ok := Presets[*name]
	if !ok || fs.NArg() != 0 || *root == "" {
		_, _ = fmt.Fprintln(stderr, "known --profile and new --root are required")
		return 2
	}
	r := result{Profile: p, Verdict: "invalid"}
	if err := requireIsolation(p); err != nil {
		r.invalidate(containerLaunchFailed, err)
		return emitResult(stdout, r)
	}
	resolved, err := exec.LookPath(*binary)
	if err != nil {
		r.invalidate(daemonHealthFailed, err)
		return emitResult(stdout, r)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		r.invalidate(daemonHealthFailed, err)
		return emitResult(stdout, r)
	}
	instanceRoot, err := filepath.Abs(*root)
	if err != nil {
		r.invalidate(containerLaunchFailed, err)
		return emitResult(stdout, r)
	}
	if _, err := os.Lstat(instanceRoot); !os.IsNotExist(err) {
		r.invalidate(containerLaunchFailed, fmt.Errorf("root must not exist"))
		return emitResult(stdout, r)
	}
	ctx, cancel := context.WithTimeout(ctx, p.RampWindow+p.Duration+drainWindow+time.Minute)
	defer cancel()
	b, reason, err := startBackend(ctx, resolved, instanceRoot, p)
	if err != nil {
		r.invalidate(reason, err)
		return emitResult(stdout, r)
	}
	defer b.daemon.stop()
	defer b.pressure.stop()
	defer func() { _ = b.decisions.Close() }()
	r = run(ctx, p, b, wallClock{})
	return emitResult(stdout, r)
}

func emitResult(w io.Writer, r result) int {
	if err := json.NewEncoder(w).Encode(r); err != nil {
		return 2
	}
	switch r.Verdict {
	case "pass":
		return 0
	case "fail":
		return 1
	default:
		return 2
	}
}

func listOptions(now time.Time) readservice.RunListOptions {
	return readservice.RunListOptions{Gaggle: "demo", Workflow: "soak", Since: now.Add(-time.Hour), Until: now, ShowNoWork: true}
}
