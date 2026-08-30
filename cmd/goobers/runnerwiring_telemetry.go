package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/version"
)

// buildTelemetryClient constructs the OTel client that spans the runner walk
// (run/task/gate) and scheduler decisions, writing completed spans under
// RunsDir via JournalSpanExporter (issue #126) — the same run journal
// layout goobers trace/telemetry read back through the rollup. Shared by
// up.go/run.go exactly like buildRunnerConfig; each caller owns calling
// Shutdown on the returned client once it's done driving runs.
func buildTelemetryClient(
	ctx context.Context,
	l instance.Layout,
	scrubber journal.Scrubber,
	registry *journal.RegistryScrubber,
	otlp instance.OTLPConfig,
	stores credentials.StoreResolver,
) (*telemetry.Client, error) {
	cfg := telemetry.Config{
		ServiceName:    "goobers",
		ServiceVersion: version.Get().Version,
		BuildCommit:    version.Get().Commit,
		SpanExporter:   telemetry.NewPerGaggleJournalSpanExporter(l.Root, scrubber),
		Scrubber:       scrubber,
		Batch:          true,
	}
	if otlp.Enabled() {
		headers, err := resolveOTLPHeaders(ctx, otlp.Headers, registry, stores)
		if err != nil {
			return nil, err
		}
		cfg.Exporter = telemetry.ExporterOTLP
		cfg.OTLPEndpoint = otlp.Endpoint
		cfg.OTLPInsecure = otlp.Insecure
		cfg.OTLPHeaders = headers
		if otlp.TLS != nil {
			cfg.OTLPCAFile = otlp.TLS.CAFile
			cfg.OTLPServerName = otlp.TLS.ServerName
			cfg.OTLPCertFile = otlp.TLS.CertFile
			cfg.OTLPKeyFile = otlp.TLS.KeyFile
		}
	}
	// telemetry.New may return a non-nil *Client alongside an error wrapping
	// telemetry.ErrOTLPUnavailable (invalid TLS material) — that Client is
	// still usable for local-only telemetry, so callers must not treat every
	// non-nil error here as a construction failure. See daemon.go's call
	// site for the degrade handling.
	return telemetry.New(ctx, cfg)
}

func resolveOTLPHeaders(
	ctx context.Context,
	headerRefs map[string]instance.TokenRef,
	registry *journal.RegistryScrubber,
	stores credentials.StoreResolver,
) (map[string]string, error) {
	names := make([]string, 0, len(headerRefs))
	for name := range headerRefs {
		names = append(names, name)
	}
	sort.Strings(names)

	refs := make([]credentials.TokenRef, 0, len(names))
	for _, name := range names {
		refs = append(refs, headerRefs[name].CredentialTokenRef("telemetry.otlp.headers."+strings.ToLower(name)))
	}
	resolver, err := credentials.NewResolverWithStores(refs, stores)
	if err != nil {
		return nil, fmt.Errorf("configure telemetry OTLP headers: %w", err)
	}

	headers := make(map[string]string, len(names))
	for i, name := range names {
		value, err := resolver.Resolve(ctx, refs[i].Name)
		if err != nil {
			return nil, fmt.Errorf("resolve telemetry OTLP header %q: %w", name, err)
		}
		registry.Register([]byte(value))
		headers[name] = value
	}
	return headers, nil
}

// teeRegistrar forwards every registered secret to BOTH a run's own
// SecretRegistrar (feeding that run's journal scrubber) and the instance-global
// shared registry (feeding the span exporter + instance log). It is how a
// per-run secret reaches the two instance-lifetime consumers without changing
// internal/runner's per-run registrar creation (#117 Piece B).
type teeRegistrar struct {
	run    runner.SecretRegistrar
	shared *journal.RegistryScrubber
}

func (t teeRegistrar) Register(secret []byte) {
	t.run.Register(secret)
	t.shared.Register(secret)
}

// ingestRunTelemetry incrementally ingests one finished run, plus a refresh
// of the scheduler decision log and spans, into the local telemetry rollup (issues
// #127/#128) — internal/telemetry/rollup/ingest.go's own doc comment already
// claimed IngestRun is meant to hook a run's completion ("call it once a run
// finishes"), but nothing in cmd/goobers ever called it; every `goobers
// telemetry`/`trace` query instead paid for a full rollup.Rebuild (an
// os.Remove + full rescan) just to stay correct, and scheduler/events.jsonl
// (trigger.fired/tick.skipped/claim.*) was never ingested at all. Called from
// both up.go (every scheduler-dispatched and resumed run) and run.go (the
// one-shot manual run — its scheduler log ingest is a no-op there, since
// `goobers run` never dispatches through the scheduler), regardless of the
// run's own error, so a failed run's errors/stage_attempts still show up in
// `goobers telemetry errors`. Best-effort: the rollup is derived state, never
// the source of truth, so an ingest failure here must never fail the run
// itself.
//
// FlushLocal MUST run before IngestRun reads spans.jsonl: the local journal
// exporter batches completed spans, but flushing the whole provider would also
// wait for a configured remote collector and delay scheduler-slot release.
func ingestRunTelemetry(tel *telemetry.Client, db *rollup.DB, watermarks *intake.Store, l instance.Layout, runID string, log *journal.InstanceLog) {
	if tel != nil {
		if err := tel.FlushLocal(context.Background()); err != nil {
			logIngestFailure(log, runID, "telemetry_flush_failed", err)
		}
	}
	if db == nil {
		return
	}
	// Best-effort (the rollup is derived state, never the source of truth,
	// so a failure here must never fail the run) does NOT mean silent
	// (issue #246): a swallowed error here — e.g. the harness_transcripts PK
	// conflict on re-ingesting a resumed run — left the rollup silently
	// stale with nothing but a blank `_ =` to show for it. logIngestFailure
	// records it to the instance log, matching resumeInterruptedRuns' own
	// resume_unresolvable_workflow convention, without changing the
	// swallow-and-continue control flow.
	if err := db.IngestRun(context.Background(), filepath.Join(l.RunsDir(), runID)); err != nil {
		logIngestFailure(log, runID, "telemetry_ingest_run_failed", err)
	}
	ingestSchedulerLog(db, l.SchedulerDir(), log, runID)
	recordRunIntake(watermarks, l, runID, log)
}

// recordRunIntake records a source watermark rather than projecting inline
// (#1922/#1923, §6.1).
//
// # The inversion
//
// This used to call ProjectRunDir here, from the writer, in-process, at run
// completion — the same coupling `IngestRun` has above. That dependency does not
// survive separating execution from serving, and it has a defect that shows up
// long before any separation: a run written while the daemon is down is never
// projected at all, and nothing notices.
//
// Now the writer records "this run advanced to sequence N" and forgets about it.
// The projector discovers the marker on its own schedule and applies it. A
// marker written while the daemon is down is still there when it starts, so the
// restart pass picks it up — which is the whole point.
//
// # Why it reads the journal for the sequence
//
// The watermark must carry a REAL sequence, not a placeholder. The
// acknowledgement guard is `source_seq <= projectedSeq`: if every writer
// recorded zero, an append racing a projection would be acknowledged away as
// though it had been applied. One journal read here replaces the much larger
// whole-run ingest that used to happen at this point.
//
// # Failure is degradation, not an error
//
// Best-effort but never silent (#246). A watermark that fails to record means
// that run is discovered by the repair sweep instead — slower, but complete. The
// alternative, failing a run because a read-model hint could not be written,
// would make the read model an availability dependency of execution.
func recordRunIntake(watermarks *intake.Store, l instance.Layout, runID string, log *journal.InstanceLog) {
	if watermarks == nil {
		return
	}
	dir, err := l.FindRunDir(runID)
	if err != nil {
		logIngestFailure(log, runID, "read_model_run_dir_failed", err)
		return
	}
	seq, err := lastJournalSeq(dir)
	if err != nil {
		logIngestFailure(log, runID, "read_model_intake_seq_failed", err)
		return
	}
	if err := watermarks.Observed(context.Background(), runID, seq); err != nil {
		logIngestFailure(log, runID, "read_model_intake_failed", err)
	}
}

func runIntakeObserver(watermarks *intake.Store, log *journal.InstanceLog) func(string, uint64) {
	if watermarks == nil {
		return nil
	}
	return func(runID string, seq uint64) {
		if err := watermarks.Observed(context.Background(), runID, seq); err != nil {
			logIngestFailure(log, runID, "read_model_intake_failed", err)
		}
	}
}

// lastJournalSeq reports the highest sequence in a run's journal.
//
// Takes the maximum rather than the last record's sequence: the live instance's
// journals contain duplicate and regressed sequences (1,394 duplicates and 119
// regressions, from #530), so "the last line" and "the highest sequence" are not
// the same number. The watermark has to be the highest, or the acknowledgement
// guard would let a projection at the true maximum acknowledge a marker that
// still represented unapplied work.
func lastJournalSeq(dir string) (uint64, error) {
	reader, err := journal.OpenRead(dir)
	if err != nil {
		return 0, err
	}
	events, err := reader.Events()
	if err != nil {
		return 0, err
	}
	var highest uint64
	for _, event := range events {
		if event.Seq > highest {
			highest = event.Seq
		}
	}
	return highest, nil
}

func ingestSchedulerTelemetry(ctx context.Context, tel *telemetry.Client, db *rollup.DB, schedulerDir string, log *journal.InstanceLog) {
	if tel != nil {
		if err := tel.Flush(ctx); err != nil {
			logIngestFailure(log, "", "telemetry_flush_failed", err)
		}
	}
	if db == nil {
		return
	}
	ingestSchedulerLog(db, schedulerDir, log, "")
}

func ingestSchedulerLog(db *rollup.DB, schedulerDir string, log *journal.InstanceLog, runID string) {
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
		logIngestFailure(log, runID, "telemetry_ingest_scheduler_log_failed", err)
	}
}

// logIngestFailure appends a best-effort diagnostic event for a failed
// rollup ingest (issue #246) — nil-safe (log may be nil in a test/standalone
// context) and itself swallows its own Append error, since a logging
// failure must not cascade into a second failure mode.
func logIngestFailure(log *journal.InstanceLog, runID, code string, cause error) {
	if log == nil {
		return
	}
	_ = log.Append(journal.Event{
		Type: journal.EventError, RunID: runID,
		Error: &journal.ErrorDetail{Code: code, Message: cause.Error()},
	})
}
