// Command scale is the committed load/scale harness for the portal read path
// (#1913, epic #1912 — the portal read architecture; originally #1416/#1410).
//
// It does two things: it synthesizes a Goobers instance at a parameterizable
// scale, and it samples the read/ingest/reconcile paths the portal depends on so
// we can show the portal stays responsive as history grows and guard against
// regressions.
//
// The generator writes every run through the production journal.Create/Append/
// Record* API, so the on-disk format tracks schema evolution automatically — a
// format change that breaks the daemon breaks this harness too. The scheduler
// journal is written directly in the canonical envelope because
// journal.OpenInstanceLog.Append re-reads the whole log per append (O(n²), the
// defect #1914 fixes) and cannot build a large journal in reasonable time.
//
// # What "1×" means, and why it is dated
//
// 1× is the live self-hosting instance as measured on 2026-07-29 (design
// docs/design/portal-read-architecture.md §2): 29,759 published run directories
// plus 10,906 unpublished ones, 191 MB of run events, 2,263 MB of spans, and a
// 324 MB scheduler journal across 156,765 records. See the baseline constants in
// generate.go for the full table and for what each pathology models.
//
// The previous constants described a smaller, earlier instance and had drifted to
// roughly 4× off on journal bytes with nothing to catch it, so every "1×" number
// understated the thing it claimed to reproduce. TestScaledSpecReproducesLiveShape
// now pins them. **They will drift again** — re-anchor deliberately with a new
// measurement rather than letting 1× quietly stop meaning anything.
//
// # Sampling and what may be claimed from it
//
// Each read path is sampled -samples times. The first sample is reported as
// cold (it pays for cache population and index reconciliation) and the rest as
// the warm distribution, because design §14.12 states separate cold and warm
// targets and pooling them hides the cost that matters on a cold start.
//
// A p99.9 is only reported when there are at least 1,000 warm samples. Below
// that the nearest rank to 0.999 is simply the maximum, so "p99.9" would be the
// max wearing a name that implies a tail estimate — and §14.12's targets are
// meant to be falsifiable. The report prints n/a and says why.
//
// # Running the harness
//
// A quick local smoke (generated and measured in a scratch dir that is removed
// afterward):
//
//	go run ./test/scale -scale=0.01 -measure
//
// A scale point against a persisted instance directory, kept for inspection:
//
//	go run ./test/scale -scale=1  -out=/tmp/scale-1x  -measure -json=/tmp/1x.json
//	go run ./test/scale -scale=3  -out=/tmp/scale-3x  -measure -json=/tmp/3x.json
//	go run ./test/scale -scale=10 -out=/tmp/scale-10x -measure -json=/tmp/10x.json
//
// -json writes a machine-readable measurement, which is what makes §16's
// "publish the baseline before changing anything" checkable rather than a claim:
// two JSON reports diff, a wall of text does not.
//
// To publish a p99.9, ask for enough samples:
//
//	go run ./test/scale -scale=1 -out=/tmp/scale-1x -measure -samples=1000
//
// # Generation cost, and the fsync knob
//
// Generation is fsync-bound, not CPU-bound: measured 6.9 runs/s with fsync on
// versus 124 runs/s with GOOBERS_DISABLE_FSYNC=1 — the difference between a
// four-hour and a thirteen-minute 100k-run corpus. Disabling it is usually the
// right call for a read-path measurement, but it changes the durability behavior
// some fixtures exist to exercise, so the report records it in the host line and
// in the JSON rather than leaving it to be inferred.
//
//	GOOBERS_DISABLE_FSYNC=1 go run ./test/scale -runs=100000 -scheduler-events=520000 \
//	    -out=/data/scale-100k -measure -samples=1000 -json=/data/100k.json
//
// The correctness tests (fast, always on) and the opt-in target-scale
// measurement live in scale_test.go. This binary is test/benchmark
// infrastructure and is never built into production paths.
package main

import (
	"encoding/json"
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
	samples         int
	jsonOut         string
	workflows       int
	gagglesFlag     int
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		return 2
	}

	spec := scaledSpec("", opts.scale)
	if opts.runs > 0 {
		spec.Runs = opts.runs
		// Orphan directories and giant scheduler records are *proportions* of the
		// live instance's shape, so pinning the run count has to re-derive them.
		// Leaving them at the -scale value silently produces a corpus with the
		// wrong unpublished-directory fraction — which is the pathology that makes
		// a directory sweep expensive, so getting it wrong quietly understates the
		// cost the harness exists to measure.
		spec.OrphanDirs = int(float64(spec.Runs) * baselineOrphanFraction)
		spec.OversizedRuns = spec.Runs / 1000
	}
	if opts.schedulerEvents > 0 {
		spec.SchedulerEvents = opts.schedulerEvents
		spec.GiantSchedulerRecords = scaleGiantRecords(spec.SchedulerEvents)
	}
	if opts.workflows > 0 {
		spec.Inventory.Workflows = opts.workflows
	}
	if opts.gagglesFlag > 0 {
		spec.Inventory.Gaggles = opts.gagglesFlag
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

	_, _ = fmt.Fprintf(stdout, "scale: generating %d runs + %d scheduler events into %s (%d gaggles, %d workflows)\n",
		spec.Runs, spec.SchedulerEvents, root, spec.Inventory.Gaggles, spec.Inventory.Workflows)
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

	m, err := measure(gen.Layout, gen, opts.samples, fsyncDisabled())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "scale: %v\n", err)
		return 1
	}
	writeReport(stdout, m)
	if opts.jsonOut != "" {
		if err := writeJSONReport(opts.jsonOut, m); err != nil {
			_, _ = fmt.Fprintf(stderr, "scale: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "scale: wrote %s\n", opts.jsonOut)
	}
	return 0
}

// fsyncDisabled reports whether the corpus was generated with the journal's
// fsync suppressed, so the measurement can say so. Generation is ~18× faster
// that way (measured 124 vs 6.9 runs/s), which is the difference between a 13
// minute and a 4 hour 100k-run corpus — but it changes the durability behavior
// some fixtures exist to exercise, so it must be reported, never inferred.
func fsyncDisabled() bool {
	return os.Getenv("GOOBERS_DISABLE_FSYNC") == "1"
}

// writeJSONReport persists a measurement as a machine-readable baseline.
//
// The text report is for a human reading a PR; this is what makes "publish the
// baseline before changing anything" (§16) checkable rather than a claim, since
// two JSON reports can be diffed and a text block cannot.
func writeJSONReport(path string, m Measurement) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("scale: marshal measurement: %w", err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("scale: write measurement: %w", err)
	}
	return nil
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
	flags.IntVar(&opts.samples, "samples", 20, "times to sample each read path; the first is cold, the rest warm (>=1000 to state a p99.9)")
	flags.StringVar(&opts.jsonOut, "json", "", "also write the measurement to this path as JSON (a comparable baseline artifact)")
	flags.IntVar(&opts.workflows, "workflows", 0, "exact workflow count in the generated inventory (overrides -scale; 2000 is §14.4's fan-out target)")
	flags.IntVar(&opts.gagglesFlag, "gaggles", 0, "exact gaggle count in the generated inventory (overrides -scale)")
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
//
// The host line is not decoration. §14.12's targets are declared "on the
// reference benchmark host" and none was ever defined, so a number without its
// hardware cannot be compared to a target or to another run.
func writeReport(w io.Writer, m Measurement) {
	_, _ = fmt.Fprintf(w, "scale report: runs=%d orphan_dirs=%d scheduler_events=%d\n", m.Runs, m.OrphanDirs, m.SchedulerEvents)
	_, _ = fmt.Fprintf(w, "  host                %s\n", m.Host)
	_, _ = fmt.Fprintf(w, "  run_events          %s\n", humanBytes(m.RunEventsSize))
	_, _ = fmt.Fprintf(w, "  spans               %s (%s events)\n", humanBytes(m.SpansSize), spanEventRatio(m))
	_, _ = fmt.Fprintf(w, "  scheduler_journal   %s\n", humanBytes(m.SchedulerJournalSize))
	_, _ = fmt.Fprintf(w, "  telemetry_db        %s\n", humanBytes(m.TelemetryDBSize))
	_, _ = fmt.Fprintf(w, "  rollup_rebuild      %s\n", m.RollupRebuild.Round(time.Millisecond))
	for _, s := range m.Stats {
		p999 := "n/a"
		if s.P999Valid {
			p999 = s.P999.Round(time.Microsecond).String()
		}
		_, _ = fmt.Fprintf(w, "  %-24s cold=%-10s p50=%-10s p99=%-10s p99.9=%-10s max=%-10s n=%d\n",
			s.Op,
			s.Cold.Round(time.Microsecond),
			s.P50.Round(time.Microsecond),
			s.P99.Round(time.Microsecond),
			p999,
			s.Max.Round(time.Microsecond),
			s.Samples,
		)
	}
	if !allP999Valid(m) {
		_, _ = fmt.Fprintf(w, "  note: p99.9 requires >= %d warm samples (-samples); reported as n/a below that\n", minSamplesForP999)
	}
}

// spanEventRatio expresses the span:event byte ratio the corpus achieved. The
// live instance measures ~12×; a corpus far from that is not modelling the
// instance whose numbers the targets came from.
func spanEventRatio(m Measurement) string {
	if m.RunEventsSize == 0 {
		return "n/a ×"
	}
	return fmt.Sprintf("%.1f×", float64(m.SpansSize)/float64(m.RunEventsSize))
}

// allP999Valid reports whether every sampled stat carried enough warm samples to
// state a p99.9.
func allP999Valid(m Measurement) bool {
	for _, s := range m.Stats {
		if !s.P999Valid {
			return false
		}
	}
	return true
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
