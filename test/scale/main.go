// Command scale is the committed load/scale harness for the telemetry read path
// (issue #1416, epic #1410 — dashboard resilience at 10–100× scale).
//
// It does two things: it synthesizes a Goobers instance at a parameterizable
// scale, and it benchmarks the telemetry read/ingest/reconcile paths the
// dashboard depends on so we can prove the dashboard stays responsive at 10–100×
// the current dogfood instance (~13.6k runs behind a ~290 MB scheduler journal)
// and guard against regressions.
//
// The generator writes every run through the production journal.Create/Append/
// Record* API, so the on-disk format tracks schema evolution automatically — a
// format change that breaks the daemon breaks this harness too. The scheduler
// journal is written directly in the canonical envelope because
// journal.OpenInstanceLog.Append re-reads the whole log per append (O(n²)) and
// cannot build a large journal in reasonable time. Injected pathologies (orphan
// run directories with no run.yaml, oversized records) exercise resilience.
//
// # Running the harness
//
// A quick local smoke (a few hundred runs, generated and measured in a scratch
// dir that is removed afterward):
//
//	go run ./test/scale -scale=0.01 -measure
//
// A target-scale run against a persisted instance directory (kept for
// inspection). Scale is a multiplier over the dogfood baseline, so -scale=1
// reproduces today's instance and -scale=10 / -scale=100 are the resilience
// targets:
//
//	go run ./test/scale -scale=1  -out=/tmp/scale-1x   -measure
//	go run ./test/scale -scale=10 -out=/tmp/scale-10x  -measure
//
// To generate 100k+ runs, drive the run count directly (each run is a real
// journal directory with per-append fsync, so generation is I/O-bound — expect
// this to take minutes to tens of minutes and gigabytes of disk):
//
//	go run ./test/scale -runs=100000 -scheduler-events=3000000 -out=/data/scale-100k -measure
//
// The correctness test (fast, always on) and the opt-in target-scale latency
// benchmark live in scale_test.go; see that file for the CI story. This binary
// is test/benchmark infrastructure and is never built into production paths.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// options are the harness CLI knobs. -scale is a convenience multiplier over the
// dogfood baseline; the explicit -runs/-scheduler-events flags override it when
// non-zero so an exact scale point can be pinned.
type options struct {
	scale           float64
	runs            int
	schedulerEvents int
	out             string
	seed            int64
	measure         bool
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		return 2
	}

	spec := scaledSpec("", opts.scale)
	if opts.runs > 0 {
		spec.Runs = opts.runs
	}
	if opts.schedulerEvents > 0 {
		spec.SchedulerEvents = opts.schedulerEvents
	}
	spec.Seed = opts.seed

	// A persisted -out is kept for inspection; without one, generate into a
	// scratch dir and remove it afterward so a smoke run leaves no trace.
	root := opts.out
	ephemeral := root == ""
	if ephemeral {
		root, err = os.MkdirTemp("", "goobers-scale-")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "scale: create scratch dir: %v\n", err)
			return 1
		}
		defer func() { _ = os.RemoveAll(root) }()
	}
	spec.Root = root

	_, _ = fmt.Fprintf(stdout, "scale: generating %d runs + %d scheduler events into %s\n",
		spec.Runs, spec.SchedulerEvents, root)
	gen, err := generate(spec)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "scale: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "scale: generated in %s; scheduler journal %s\n",
		gen.Elapsed.Round(time.Millisecond), humanBytes(gen.SchedulerJournalSize))

	if !opts.measure {
		return 0
	}

	m, err := measure(gen.Layout, gen.Runs, gen.SchedulerEvents, gen.SchedulerJournalSize)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "scale: %v\n", err)
		return 1
	}
	writeReport(stdout, m)
	return 0
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	flags := flag.NewFlagSet("scale", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts options
	flags.Float64Var(&opts.scale, "scale", 0.01, "size multiplier over the dogfood baseline (1 = current instance, 10/100 = targets)")
	flags.IntVar(&opts.runs, "runs", 0, "exact run count (overrides -scale when > 0)")
	flags.IntVar(&opts.schedulerEvents, "scheduler-events", 0, "exact scheduler event count (overrides -scale when > 0)")
	flags.StringVar(&opts.out, "out", "", "instance directory to populate and keep (default: a removed scratch dir)")
	flags.Int64Var(&opts.seed, "seed", 1, "deterministic generation seed")
	flags.BoolVar(&opts.measure, "measure", false, "after generating, benchmark the read/ingest/reconcile paths")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if opts.scale <= 0 {
		_, _ = fmt.Fprintln(stderr, "scale: -scale must be positive")
		return options{}, fmt.Errorf("invalid scale")
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/scale [-scale f] [-runs n] [-scheduler-events n] [-out dir] [-seed n] [-measure]")
		return options{}, fmt.Errorf("unexpected positional arguments")
	}
	return opts, nil
}

// writeReport prints the measured latencies and footprint in a compact,
// grep-friendly form — the numbers to paste into a PR or compare across scales.
func writeReport(w io.Writer, m Measurement) {
	_, _ = fmt.Fprintf(w, "scale report: runs=%d scheduler_events=%d\n", m.Runs, m.SchedulerEvents)
	_, _ = fmt.Fprintf(w, "  scheduler_journal   %s\n", humanBytes(m.SchedulerJournalSize))
	_, _ = fmt.Fprintf(w, "  telemetry_db        %s\n", humanBytes(m.TelemetryDBSize))
	_, _ = fmt.Fprintf(w, "  rollup_rebuild      %s\n", m.RollupRebuild.Round(time.Millisecond))
	_, _ = fmt.Fprintf(w, "  listruns_first_page %s\n", m.ListRunsFirstPage.Round(time.Microsecond))
	_, _ = fmt.Fprintf(w, "  listruns_warm_page  %s\n", m.ListRunsWarmPage.Round(time.Microsecond))
	_, _ = fmt.Fprintf(w, "  overview_fanout     %s\n", m.OverviewFanout.Round(time.Microsecond))
	_, _ = fmt.Fprintf(w, "  status_full_scan    %s\n", m.StatusScan.Round(time.Millisecond))
}

// humanBytes formats a byte count in the largest sensible binary unit.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
