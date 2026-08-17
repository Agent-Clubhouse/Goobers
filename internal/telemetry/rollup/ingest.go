package rollup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// IngestRun reads a single run directory's journal (run.yaml + events.jsonl)
// and the telemetry span exporter's spans/spans.jsonl, and (re)populates the
// rollup's rows for that run. Ingestion is idempotent: existing rows for the
// run are deleted before the fresh insert, so IngestRun doubles as both
// incremental ingestion (call it once a run finishes — "hooks the runner",
// TEL-032) and the per-run primitive Rebuild uses to rederive the whole store
// from the journals (the rollup is derived state, never the source of truth).
func (db *DB) IngestRun(ctx context.Context, runDir string) error {
	return journal.WithPruneProtection(runDir, func() error {
		return db.ingestRun(ctx, runDir)
	})
}

func (db *DB) ingestRun(ctx context.Context, runDir string) error {
	identity, err := readRunIdentity(runDir)
	if err != nil {
		return err
	}
	events, err := readEvents(runDir)
	if err != nil {
		return err
	}
	spans, err := readSpans(runDir)
	if err != nil {
		return err
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rollup: begin ingest tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	runID := identity.RunID
	if err := deleteRun(ctx, tx, runID); err != nil {
		return err
	}
	if err := insertRun(ctx, tx, identity, events); err != nil {
		return err
	}
	if err := insertEvents(ctx, tx, runID, events); err != nil {
		return err
	}
	if err := insertCICheckFailures(ctx, tx, runDir, runID, events); err != nil {
		return err
	}
	if err := insertBacklogProjections(tx, runDir, identity, events); err != nil {
		return err
	}
	if err := insertSpans(ctx, tx, runID, spans); err != nil {
		return err
	}
	if err := upsertTimeToFirstPR(ctx, tx, time.Time{}, runFirstPROpenAt(events)); err != nil {
		return err
	}
	return tx.Commit()
}

// perRunTables lists every table keyed by run_id that IngestRun populates —
// deleteRun must clear all of them before a fresh insert, or a re-ingest
// (e.g. after a daemon restart resumes a run that already ingested once,
// issue #246) hits a stale row's primary key and rolls back the whole
// transaction. TestDeleteRunCoversEverySchemaTable guards against the next
// table added to insertEvents/insertSpans silently repeating this gap.
var perRunTables = []string{"runs", "run_goober_digests", "stage_attempts", "stage_usage", "agent_invocations", "stage_model_usage", "gate_verdicts", "provider_mutations", "run_errors", "ci_check_failures", "spans", "span_events", "harness_transcripts", "harness_transcript_schemas", "span_business_status", "curation_actions", "ready_pool_samples", "ready_claims", "ready_label_transitions"}

func deleteRun(ctx context.Context, tx *sql.Tx, runID string) error {
	for _, table := range perRunTables {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE run_id = ?`, table), runID); err != nil {
			return fmt.Errorf("rollup: clear %s for run %s: %w", table, runID, err)
		}
	}
	return nil
}

// DeleteRun removes every rollup row derived from one run in a single
// transaction. The caller coordinates deletion of the source journal.
func (db *DB) DeleteRun(ctx context.Context, runID string) error {
	if runID == "" {
		return fmt.Errorf("rollup: run id is required")
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rollup: begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteRun(ctx, tx, runID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rollup: commit delete for run %s: %w", runID, err)
	}
	checkpointWAL(db.sql)
	return nil
}

func insertRun(ctx context.Context, tx *sql.Tx, id runIdentity, events []journalEvent) error {
	var status string
	var finishedAt time.Time
	for _, ev := range events {
		switch ev.Type {
		case eventRunResumed:
			status = ""
			finishedAt = time.Time{}
		case eventRunFinished:
			status = ev.Status
			finishedAt = ev.Time
		}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO runs (run_id, workflow, workflow_version, workflow_digest, gaggle, trigger_kind, trigger_ref, status, started_at, finished_at, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.RunID, id.Workflow, id.WorkflowVersion, nullIfEmpty(id.WorkflowDigest), id.Gaggle,
		nullIfEmpty(id.Trigger.Kind), nullIfEmpty(id.Trigger.Ref), nullIfEmpty(status),
		formatTime(id.StartedAt), formatTime(finishedAt), durationMillis(id.StartedAt, finishedAt))
	if err != nil {
		return fmt.Errorf("rollup: insert run %s: %w", id.RunID, err)
	}
	if id.GooberDigest != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO run_goober_digests (run_id, goober_digest)
			VALUES (?, ?)`, id.RunID, id.GooberDigest); err != nil {
			return fmt.Errorf("rollup: insert goober digest for run %s: %w", id.RunID, err)
		}
	}
	return nil
}

// stageAttemptAccum consolidates fields spread across a stage attempt's
// stage.started / stage.finished / (optional) error events into one row.
type stageAttemptAccum struct {
	attempt               int
	attemptClass          string
	status                string
	startedAt, finishedAt time.Time
	errorCode, errorClass string
	runnerJSON            sql.NullString
}

type stageKey struct {
	stage     string
	branch    int
	traversal int
}

type stageAttemptKey struct {
	stage   string
	branch  int
	attempt int
}

func insertEvents(ctx context.Context, tx *sql.Tx, runID string, events []journalEvent) error {
	stages := map[stageKey]*stageAttemptAccum{}
	var order []stageKey
	current := map[stageAttemptKey]stageKey{}
	traversals := map[string]int{}
	eventStageKeys := make([]stageKey, len(events))
	newAccum := func(stage string, branch, attempt int) stageKey {
		traversals[stage]++
		k := stageKey{stage: stage, branch: branch, traversal: traversals[stage]}
		stages[k] = &stageAttemptAccum{attempt: attempt}
		order = append(order, k)
		current[stageAttemptKey{stage: stage, branch: branch, attempt: attempt}] = k
		return k
	}

	for i, ev := range events {
		if ev.Stage == "" {
			continue
		}
		attemptKey := stageAttemptKey{stage: ev.Stage, branch: ev.Branch, attempt: ev.Attempt}
		switch ev.Type {
		case eventStageStarted:
			eventStageKeys[i] = newAccum(ev.Stage, ev.Branch, ev.Attempt)
		case eventStageFinished:
			k, ok := current[attemptKey]
			if !ok {
				k = newAccum(ev.Stage, ev.Branch, ev.Attempt)
			}
			eventStageKeys[i] = k
		case eventError:
			// Stage-scoped terminal diagnostics such as run_failed do not
			// identify an attempt and must not create a synthetic traversal.
			if ev.Attempt == 0 {
				continue
			}
			k, ok := current[attemptKey]
			if !ok {
				k = newAccum(ev.Stage, ev.Branch, ev.Attempt)
			}
			eventStageKeys[i] = k
		}
	}

	// Pre-scan standalone error codes per stage/attempt so the
	// eventStageFinished case below can dedupe against them (issue #230)
	// regardless of where each event falls in the slice — a standalone
	// error event and stage.finished's own inline error are recorded for
	// genuinely different faults in practice (worktree_remove_failed vs a
	// business failure code) and both are wanted, but if the same code ever
	// shows up both ways for the same attempt, it must count once.
	standaloneErrorCodes := map[stageKey]map[string]bool{}
	for i, ev := range events {
		if ev.Type == eventError && ev.Error != nil && ev.Stage != "" && ev.Attempt != 0 {
			k := eventStageKeys[i]
			if standaloneErrorCodes[k] == nil {
				standaloneErrorCodes[k] = map[string]bool{}
			}
			code, _ := errorCodeAndClass(ev)
			standaloneErrorCodes[k][code] = true
		}
	}

	for i, ev := range events {
		switch ev.Type {
		case eventStageStarted:
			a := stages[eventStageKeys[i]]
			if ev.AttemptClass != "" {
				a.attemptClass = ev.AttemptClass
			}
			a.startedAt = ev.Time
			if rj, err := runnerJSON(ev.Runner); err != nil {
				return err
			} else if rj.Valid {
				a.runnerJSON = rj
			}

		case eventStageFinished:
			k := eventStageKeys[i]
			a := stages[k]
			a.status = ev.Status
			a.finishedAt = ev.Time
			if rj, err := runnerJSON(ev.Runner); err != nil {
				return err
			} else if rj.Valid {
				a.runnerJSON = rj
			}
			// Business failures (nonzero_exit, timeout, missing_result_file,
			// exec_start, result_file_path_escape) are recorded ONLY inline
			// here, never as a standalone error event (architect ruling,
			// #230) — derive a run_errors row from them the same way the
			// eventError case below does, or `telemetry errors` silently
			// misses this entire failure class despite `telemetry stats`
			// correctly counting it.
			if ev.Status == stageStatusFailure && ev.Error != nil {
				class := string(telemetry.ClassifyError(ev.Error.Code))
				a.errorCode = ev.Error.Code
				a.errorClass = class
				if !standaloneErrorCodes[k][ev.Error.Code] {
					if _, err := tx.ExecContext(ctx, `
						INSERT INTO run_errors (run_id, seq, stage, attempt, code, error_class, message, occurred_at)
						VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
						runID, ev.Seq, nullIfEmpty(ev.Stage), nullIfZeroInt(ev.Attempt), ev.Error.Code,
						nullIfEmpty(class), nullIfEmpty(capMessage(telemetry.Redact(ev.Error.Message))), formatTime(ev.Time)); err != nil {
						return fmt.Errorf("rollup: insert run_error (stage.finished) seq %d: %w", ev.Seq, err)
					}
				}
			}

		case eventError:
			if ev.Error == nil {
				continue
			}
			code, class := errorCodeAndClass(ev)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO run_errors (run_id, seq, stage, attempt, code, error_class, message, occurred_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				runID, ev.Seq, nullIfEmpty(ev.Stage), nullIfZeroInt(ev.Attempt), code,
				nullIfEmpty(class), nullIfEmpty(capMessage(telemetry.Redact(ev.Error.Message))), formatTime(ev.Time)); err != nil {
				return fmt.Errorf("rollup: insert run_error seq %d: %w", ev.Seq, err)
			}
			if a := stages[eventStageKeys[i]]; a != nil {
				a.errorCode = code
				a.errorClass = class
				// The producer's own diagnostic context (retry classification,
				// whether the failed attempt committed work, #2224) is on the
				// error event, not on stage.started — without this the failing
				// attempt's runner_json was NULL for exactly the failures an
				// operator most needs it for.
				if rj, err := runnerJSON(ev.Runner); err != nil {
					return err
				} else if rj.Valid {
					a.runnerJSON = rj
				}
				// Dispatch failures are followed directly by the next
				// stage.started event, without a stage.finished event. The
				// error event is therefore the journal's attempt boundary.
				// Keyed on the journal's own Error.Code, which stays
				// executor_error for every dispatch failure — the typed cause
				// resolved above refines the row, it does not redefine which
				// events close an attempt.
				if ev.Error.Code == "executor_error" && a.finishedAt.IsZero() {
					a.status = stageStatusFailure
					a.finishedAt = ev.Time
				}
			}

		case eventGateEvaluated:
			// Runner{repassAttempt, escalated} plus a pointer at the verdict
			// artifact (decision/rationale/evidence, for agentic gates —
			// internal/gate/journal.go's recordVerdict) is exactly the
			// "gate X failed 3 repasses then escalated" signal Tutor/
			// nomination need (TUT-010 gate-noise family, issue #128) — v1
			// discarded both, leaving runner_json permanently NULL.
			rj, err := gateRunnerJSON(ev)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO gate_verdicts (run_id, seq, gate, verdict, target, occurred_at, runner_json, branch)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				runID, ev.Seq, ev.Gate, nullIfEmpty(telemetry.Redact(ev.Verdict)), nullIfEmpty(ev.Target), formatTime(ev.Time), rj, ev.Branch); err != nil {
				return fmt.Errorf("rollup: insert gate_verdict seq %d: %w", ev.Seq, err)
			}

		case eventSpanRecorded:
			// Within-stage harness data (agent transcripts, tool output —
			// GBO-020) that v1 recorded to the journal via
			// journal.Run.RecordSpan but the rollup never ingested: the blob
			// itself stays content-addressed in the run journal's spans/
			// store (§3.3 excludes it from conformance, and it's often large
			// live-harness output) — this is a queryable pointer, not a copy.
			var digest sql.NullString
			var size sql.NullInt64
			if ev.Ref != nil {
				digest = nullIfEmpty(ev.Ref.Digest)
				size = nullIfZeroInt64(ev.Ref.Size)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO harness_transcripts (run_id, seq, stage, name, ref_digest, ref_size, occurred_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				runID, ev.Seq, ev.Stage, ev.Name, digest, size, formatTime(ev.Time)); err != nil {
				return fmt.Errorf("rollup: insert harness_transcript seq %d: %w", ev.Seq, err)
			}
			if ev.DataSchema != "" {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO harness_transcript_schemas (run_id, seq, schema)
					VALUES (?, ?, ?)`, runID, ev.Seq, ev.DataSchema); err != nil {
					return fmt.Errorf("rollup: insert harness_transcript schema seq %d: %w", ev.Seq, err)
				}
			}

		case eventRefTouched:
			if ev.ExternalRef == nil {
				continue
			}
			rj, err := runnerJSON(ev.Runner)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO provider_mutations (run_id, seq, provider, kind, external_id, url, operation, occurred_at, runner_json)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				runID, ev.Seq, ev.ExternalRef.Provider, ev.ExternalRef.Kind, ev.ExternalRef.ID,
				nullIfEmpty(ev.ExternalRef.URL), nullIfEmpty(operationFromRunner(ev.Runner)), formatTime(ev.Time), rj); err != nil {
				return fmt.Errorf("rollup: insert provider_mutation seq %d: %w", ev.Seq, err)
			}
		}
	}

	for _, k := range order {
		a := stages[k]
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO stage_attempts (run_id, stage, traversal, attempt, attempt_class, status, started_at, finished_at, duration_ms, error_code, error_class, runner_json, branch)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, k.stage, k.traversal, a.attempt, nullIfEmpty(a.attemptClass), nullIfEmpty(a.status),
			formatTime(a.startedAt), formatTime(a.finishedAt), durationMillis(a.startedAt, a.finishedAt),
			nullIfEmpty(a.errorCode), nullIfEmpty(a.errorClass), a.runnerJSON, k.branch); err != nil {
			return fmt.Errorf("rollup: insert stage_attempt %s traversal %d: %w", k.stage, k.traversal, err)
		}
	}
	return nil
}

type ciChecksArtifact struct {
	Checks []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"checks"`
}

func insertCICheckFailures(ctx context.Context, tx *sql.Tx, runDir, runID string, events []journalEvent) error {
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if ev.Type != eventStageFinished || ev.Outputs["ciStatus"] != "failing" {
			continue
		}
		seen := make(map[string]bool)
		for _, ref := range ev.Artifacts {
			if ref.MediaType != "application/json" {
				continue
			}
			data, readErr := reader.ArtifactBytes(journal.Ref{
				Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: ref.MediaType,
			})
			if readErr != nil {
				return fmt.Errorf("rollup: read CI checks artifact for run %s: %w", runID, readErr)
			}
			var artifact ciChecksArtifact
			if json.Unmarshal(data, &artifact) != nil || artifact.Checks == nil {
				continue
			}
			for _, check := range artifact.Checks {
				name := strings.TrimSpace(check.Name)
				if check.State != "failing" || name == "" || seen[name] {
					continue
				}
				seen[name] = true
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO ci_check_failures (run_id, seq, stage, check_name, artifact_digest, occurred_at)
					VALUES (?, ?, ?, ?, ?, ?)`,
					runID, ev.Seq, ev.Stage, name, ref.Digest, formatTime(ev.Time)); err != nil {
					return fmt.Errorf("rollup: insert CI check failure %q for run %s: %w", name, runID, err)
				}
			}
		}
	}
	return nil
}

func backfillCICheckFailures(ctx context.Context, tx *sql.Tx, instanceRoot string) error {
	rows, err := tx.QueryContext(ctx, `SELECT run_id FROM runs`)
	if err != nil {
		return fmt.Errorf("query existing runs: %w", err)
	}
	runIDs := make(map[string]bool)
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan existing run: %w", err)
		}
		runIDs[runID] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate existing runs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close existing runs: %w", err)
	}
	if len(runIDs) == 0 {
		return nil
	}

	runsRoots := []string{filepath.Join(instanceRoot, "runs")}
	gaggles, err := os.ReadDir(filepath.Join(instanceRoot, "gaggles"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read gaggle roots: %w", err)
	}
	for _, gaggle := range gaggles {
		if gaggle.IsDir() {
			runsRoots = append(runsRoots, filepath.Join(instanceRoot, "gaggles", gaggle.Name(), "runs"))
		}
	}
	for _, runsRoot := range runsRoots {
		dirs, err := runDirs(runsRoot)
		if err != nil {
			return err
		}
		for _, runDir := range dirs {
			runID := filepath.Base(runDir)
			if !runIDs[runID] {
				continue
			}
			events, err := readEvents(runDir)
			if err != nil {
				return err
			}
			if err := insertCICheckFailures(ctx, tx, runDir, runID, events); err != nil {
				return err
			}
		}
	}
	return nil
}

// Runner-namespace keys carrying a stage failure's typed cause. The producer
// (internal/runner) writes them alongside the generic Error.Code, which stays
// executor_error for every dispatch failure because that exact string is the
// journal's attempt-boundary marker.
const (
	runnerErrorCodeKey  = "errorCode"
	runnerErrorClassKey = "errorClass"
)

// errorCodeAndClass resolves the code and class one error event contributes to
// the rollup. The producer's typed refinement wins when present: an unauthorized
// clone, a broken credential helper, a DNS outage and a claims-lock timeout are
// four different owners' problems that otherwise share the one executor_error /
// unknown row, which is what forced the 2026-08-08 audit to reconstruct a
// failure taxonomy by reading 3,487 journals by hand. Classification is never
// inferred from the message here — an unrefined event still classifies purely
// from its own code.
func errorCodeAndClass(ev journalEvent) (code, class string) {
	code = ev.Error.Code
	if typed, ok := ev.Runner[runnerErrorCodeKey].(string); ok && typed != "" {
		code = typed
	}
	class = string(telemetry.ClassifyError(code))
	if typed, ok := ev.Runner[runnerErrorClassKey].(string); ok && typed != "" {
		class = typed
	}
	return code, class
}

var curationAgentOutputKeys = []string{
	"ready",
	"needsHuman",
	"closed",
	"deduped",
	"split",
	"stale",
	"milestoned",
}

func insertBacklogProjections(tx *sql.Tx, runDir string, id runIdentity, events []journalEvent) error {
	if id.Workflow == "backlog-curation" {
		if err := insertCurationAction(context.Background(), tx, id.RunID, events); err != nil {
			return err
		}
		if err := insertReadyPoolSample(context.Background(), tx, id.RunID, events); err != nil {
			return err
		}
		if err := insertReadyLabelTransitions(context.Background(), tx, runDir, id.RunID, events); err != nil {
			return err
		}
	}
	if id.Workflow == "implementation" {
		if err := insertReadyClaims(context.Background(), tx, id.RunID, events); err != nil {
			return err
		}
	}
	return nil
}

func insertCurationAction(ctx context.Context, tx *sql.Tx, runID string, events []journalEvent) error {
	counts := make([]int, 9)
	reported := false
	status := ""
	occurredAt := time.Time{}
	for _, ev := range events {
		if ev.Type == eventRunFinished {
			status = ev.Status
			occurredAt = ev.Time
		}
		if ev.Type == eventStageFinished && ev.Stage == "reconcile-backlog" && ev.Status == stageStatusSuccess {
			if count, ok := nonnegativeOutputInt(ev.Outputs, "reconciled"); ok {
				counts[6] = count
			}
		}
		if ev.Type != eventStageFinished || ev.Stage != "curate" || ev.Status != stageStatusSuccess {
			continue
		}
		valid := true
		for i, key := range curationAgentOutputKeys {
			count, ok := nonnegativeOutputInt(ev.Outputs, key)
			if !ok {
				valid = false
				break
			}
			if i < 6 {
				counts[i] = count
			} else {
				counts[7] = count
			}
		}
		reported = valid
		if !valid {
			reconciled := counts[6]
			counts = make([]int, 9)
			counts[6] = reconciled
		}
		if occurredAt.IsZero() {
			occurredAt = ev.Time
		}
	}
	if occurredAt.IsZero() {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO curation_actions (
			run_id, status, reported, ready_count, needs_human_count, closed_count,
			deduped_count, split_count, stale_count, reconciled_count,
			milestoned_count, bounced_count, occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, nullIfEmpty(status), reported,
		counts[0], counts[1], counts[2], counts[3], counts[4],
		counts[5], counts[6], counts[7], counts[8], formatTime(occurredAt))
	if err != nil {
		return fmt.Errorf("rollup: insert curation action for run %s: %w", runID, err)
	}
	return nil
}

type readyPoolArtifact struct {
	ReadyTransitions []struct {
		EventID    int64     `json:"eventId"`
		ItemID     string    `json:"itemId"`
		Label      string    `json:"label"`
		Added      bool      `json:"added"`
		OccurredAt time.Time `json:"occurredAt"`
	} `json:"readyTransitions"`
}

func insertReadyLabelTransitions(ctx context.Context, tx *sql.Tx, runDir, runID string, events []journalEvent) error {
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if ev.Type != eventStageFinished || ev.Stage != "sample-ready-pool" || ev.Status != stageStatusSuccess {
			continue
		}
		for i := len(ev.Artifacts) - 1; i >= 0; i-- {
			ref := ev.Artifacts[i]
			if ref.MediaType != "application/json" {
				continue
			}
			data, readErr := reader.ArtifactBytes(journal.Ref{
				Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: ref.MediaType,
			})
			if readErr != nil {
				return fmt.Errorf("rollup: read ready-pool artifact for run %s: %w", runID, readErr)
			}
			var artifact readyPoolArtifact
			if unmarshalErr := json.Unmarshal(data, &artifact); unmarshalErr != nil {
				return fmt.Errorf("rollup: decode ready-pool artifact for run %s: %w", runID, unmarshalErr)
			}
			for _, transition := range artifact.ReadyTransitions {
				if transition.EventID <= 0 || transition.ItemID == "" || transition.Label == "" || transition.OccurredAt.IsZero() {
					return fmt.Errorf("rollup: invalid ready-label transition in run %s", runID)
				}
				kind := "not-ready"
				if transition.Added {
					kind = "ready"
				}
				if _, insertErr := tx.ExecContext(ctx, `
					INSERT OR IGNORE INTO ready_label_transitions (
						run_id, event_id, item_id, transition, occurred_at
					) VALUES (?, ?, ?, ?, ?)`,
					runID, transition.EventID, transition.ItemID, kind, formatTime(transition.OccurredAt)); insertErr != nil {
					return fmt.Errorf("rollup: insert ready-label transition %d: %w", transition.EventID, insertErr)
				}
			}
			break
		}
	}
	return nil
}

func insertReadyPoolSample(ctx context.Context, tx *sql.Tx, runID string, events []journalEvent) error {
	for _, ev := range events {
		if ev.Type != eventStageFinished || ev.Stage != "sample-ready-pool" || ev.Status != stageStatusSuccess {
			continue
		}
		depth, ok := nonnegativeOutputInt(ev.Outputs, "readyPoolDepth")
		average, averageOK := nonnegativeOutputFloat(ev.Outputs, "averageReadyAgeSeconds")
		oldest, oldestOK := nonnegativeOutputFloat(ev.Outputs, "oldestReadyAgeSeconds")
		observedText, observedOK := ev.Outputs["readyPoolObservedAt"].(string)
		observedAt, timeErr := time.Parse(time.RFC3339Nano, observedText)
		if !ok || !averageOK || !oldestOK || !observedOK || timeErr != nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ready_pool_samples (
				run_id, depth, average_age_seconds, oldest_age_seconds, observed_at
			) VALUES (?, ?, ?, ?, ?)`,
			runID, depth, average, oldest, formatTime(observedAt)); err != nil {
			return fmt.Errorf("rollup: insert ready-pool sample for run %s: %w", runID, err)
		}
	}
	return nil
}

func insertReadyClaims(ctx context.Context, tx *sql.Tx, runID string, events []journalEvent) error {
	for _, ev := range events {
		if ev.Type != eventStageFinished || ev.Stage != "query-backlog" || ev.Status != stageStatusSuccess {
			continue
		}
		readyText, ok := ev.Outputs["readyAt"].(string)
		if !ok {
			continue
		}
		readyAt, err := time.Parse(time.RFC3339Nano, readyText)
		if err != nil || ev.Time.Before(readyAt) {
			continue
		}
		itemID, _ := ev.Outputs["id"].(string)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ready_claims (run_id, seq, item_id, ready_age_seconds, claimed_at)
			VALUES (?, ?, ?, ?, ?)`,
			runID, ev.Seq, nullIfEmpty(itemID), ev.Time.Sub(readyAt).Seconds(), formatTime(ev.Time)); err != nil {
			return fmt.Errorf("rollup: insert ready claim seq %d: %w", ev.Seq, err)
		}
	}
	return nil
}

func nonnegativeOutputInt(outputs map[string]any, key string) (int, bool) {
	value, ok := nonnegativeOutputFloat(outputs, key)
	if !ok || math.Trunc(value) != value || value >= math.Exp2(float64(strconv.IntSize-1)) {
		return 0, false
	}
	return int(value), true
}

func nonnegativeOutputFloat(outputs map[string]any, key string) (float64, bool) {
	value, ok := outputs[key]
	if !ok {
		return 0, false
	}
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return 0, false
	}
	return number, true
}

// gateRunnerJSON merges a gate.evaluated event's Runner annotations
// (repassAttempt, escalated) with a pointer at its verdict artifact, if any
// (Name + Ref.Digest/Size — the decision/rationale/evidence blob an agentic
// gate recorded), into the one runner_json blob gate_verdicts stores. No new
// column: merging into the JSON runner_json already carries is cheaper than a
// migration and matches how stage_attempts/provider_mutations already stash
// their own runner-local detail there.
func gateRunnerJSON(ev journalEvent) (sql.NullString, error) {
	if len(ev.Runner) == 0 && ev.Ref == nil {
		return sql.NullString{}, nil
	}
	m := make(map[string]any, len(ev.Runner)+1)
	for k, v := range ev.Runner {
		m[k] = v
	}
	if ev.Ref != nil {
		m["verdictRef"] = map[string]any{"name": ev.Name, "digest": ev.Ref.Digest, "size": ev.Ref.Size}
	}
	return runnerJSON(m)
}

// schedulerCursor is the incremental-ingest watermark (#1411): how far into the
// instance journal IngestSchedulerLog has read (byteOffset) and the highest
// event seq it has applied (lastSeq).
type schedulerCursor struct {
	byteOffset int64
	lastSeq    uint64
}

// readSchedulerCursor loads the ingest watermark. With no cursor row yet — a
// fresh Rebuild, or the first ingest after upgrading from the old full-replay
// path — it seeds lastSeq from whatever scheduler_events a prior full replay
// left, so the first incremental pass re-reads the journal head once but writes
// nothing for events already stored (ON CONFLICT makes each a no-op), then
// records the cursor so every later pass reads only the new tail.
func readSchedulerCursor(ctx context.Context, sqlDB *sql.DB) (schedulerCursor, error) {
	var c schedulerCursor
	err := sqlDB.QueryRowContext(ctx, `SELECT byte_offset, last_seq FROM scheduler_ingest_cursor WHERE id = 1`).
		Scan(&c.byteOffset, &c.lastSeq)
	if err == sql.ErrNoRows {
		var seed uint64
		if err := sqlDB.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM scheduler_events`).Scan(&seed); err != nil {
			return schedulerCursor{}, fmt.Errorf("rollup: seed scheduler cursor: %w", err)
		}
		return schedulerCursor{byteOffset: 0, lastSeq: seed}, nil
	}
	if err != nil {
		return schedulerCursor{}, fmt.Errorf("rollup: read scheduler cursor: %w", err)
	}
	return c, nil
}

func writeSchedulerCursor(ctx context.Context, tx *sql.Tx, byteOffset int64, lastSeq uint64) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO scheduler_ingest_cursor (id, byte_offset, last_seq)
		VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET byte_offset = excluded.byte_offset, last_seq = excluded.last_seq`,
		byteOffset, lastSeq); err != nil {
		return fmt.Errorf("rollup: write scheduler cursor: %w", err)
	}
	return nil
}

// ResetSchedulerIngestCursor makes the next scheduler ingest re-read the
// journal from its head while preserving the sequence watermark.
func (db *DB) ResetSchedulerIngestCursor(ctx context.Context) error {
	db.schedulerMu.Lock()
	defer db.schedulerMu.Unlock()
	return db.resetSchedulerIngestCursor(ctx)
}

func (db *DB) resetSchedulerIngestCursor(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `
		INSERT INTO scheduler_ingest_cursor (id, byte_offset, last_seq)
		VALUES (1, 0, (SELECT COALESCE(MAX(seq), 0) FROM scheduler_events))
		ON CONFLICT(id) DO UPDATE SET byte_offset = 0`); err != nil {
		return fmt.Errorf("rollup: reset scheduler ingest cursor: %w", err)
	}
	return nil
}

// spansCursor is the scheduler-spans ingest watermark: how far into
// scheduler/spans/spans.jsonl IngestSchedulerLog has read. Unlike
// schedulerCursor there is no seq to skip against — the file has no analogous
// monotonic identity — so the byte offset alone is the whole contract: bytes
// before it are already ingested, everything at or after is new.
type spansCursor struct {
	byteOffset int64
}

// readSpansCursor loads the spans ingest watermark. A missing row (a fresh
// Rebuild, or the first ingest after upgrading from the old full-rescan path)
// starts at offset 0 — the next ingest re-reads the whole spans file once,
// same as every prior cycle did, and then never again: the per-span
// delete-then-insert this fix keeps is idempotent, so replaying already-stored
// spans on that one pass is harmless, just the last full-cost cycle.
func readSpansCursor(ctx context.Context, sqlDB *sql.DB) (spansCursor, error) {
	var c spansCursor
	err := sqlDB.QueryRowContext(ctx, `SELECT byte_offset FROM spans_ingest_cursor WHERE id = 1`).Scan(&c.byteOffset)
	if err == sql.ErrNoRows {
		return spansCursor{}, nil
	}
	if err != nil {
		return spansCursor{}, fmt.Errorf("rollup: read spans cursor: %w", err)
	}
	return c, nil
}

func writeSpansCursor(ctx context.Context, tx *sql.Tx, byteOffset int64) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO spans_ingest_cursor (id, byte_offset)
		VALUES (1, ?)
		ON CONFLICT(id) DO UPDATE SET byte_offset = excluded.byte_offset`,
		byteOffset); err != nil {
		return fmt.Errorf("rollup: write spans cursor: %w", err)
	}
	return nil
}

const defaultSchedulerIngestTimeout = 5 * time.Second

var errSchedulerIngestInProgress = errors.New("rollup: scheduler ingest already in progress")

// IngestSchedulerLog rolls up the instance journal (claim transitions,
// scheduler decisions, starvation and error signals) and its rolling scheduler
// spans. Both halves are incremental: a per-tick call reads only the journal
// tail past the last ingested offset and inserts only events above the seq
// watermark (#1411), and reads only the spans tail past its own byte-offset
// cursor instead of re-parsing and delete+reinserting the entire rolling spans
// file every call (telemetry storage hygiene audit, 2026-08-08 — this file
// grows for the instance's entire lifetime, unlike a run's own bounded span
// file). A steady-state ingest with nothing new performs no writes and the WAL
// stops churning. Idempotent — safe to call repeatedly or as part of Rebuild;
// INSERT ... ON CONFLICT (events) and delete-then-insert (spans) keep a
// re-read after a reset, or a historical duplicate (corruption), from ever
// duplicating a row (events: the first occurrence wins; spans: the last write
// wins, since replaying the exact same recorded span converges to the same
// row either way).
func (db *DB) IngestSchedulerLog(ctx context.Context, schedulerDir string) error {
	ctx, cancel := context.WithTimeout(ctx, db.schedulerIngestTimeout)
	defer cancel()

	// Scheduler telemetry is derived and retryable. Do not queue daemon ticks,
	// config applies, or shutdown behind an ingest already doing slow SQLite
	// cleanup; the active transaction is independently bounded above.
	if !db.schedulerMu.TryLock() {
		return errSchedulerIngestInProgress
	}
	defer db.schedulerMu.Unlock()
	return db.ingestSchedulerLog(ctx, schedulerDir)
}

func (db *DB) ingestSchedulerLog(ctx context.Context, schedulerDir string) error {
	cursor, err := readSchedulerCursor(ctx, db.sql)
	if err != nil {
		return err
	}
	events, newOffset, _, err := readInstanceEventsFrom(schedulerDir, cursor.byteOffset)
	if err != nil {
		return err
	}
	spanCursor, err := readSpansCursor(ctx, db.sql)
	if err != nil {
		return err
	}
	spans, newSpanOffset, _, err := readSchedulerSpansFrom(schedulerDir, spanCursor.byteOffset)
	if err != nil {
		return err
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rollup: begin scheduler ingest tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	// Incremental replay (#1411): the journal is append-only and immutable, so
	// events at or below the watermark are already stored. Skip them (bounds
	// the per-tick work to genuinely new events), and INSERT ... ON CONFLICT so
	// even a re-read after a reset from the head — or a torn tail that later
	// completes — never duplicates a row. No DELETE-then-reinsert of the whole
	// table: that was O(journal) writes every tick and churned the WAL.
	maxSeq := cursor.lastSeq
	for _, ev := range events {
		if ev.Seq <= cursor.lastSeq {
			continue
		}
		if ev.Seq > maxSeq {
			maxSeq = ev.Seq
		}
		switch ev.Type {
		case eventInitCompleted, eventTriggerFired, eventTickSkipped, eventProviderQuotaReset, eventPollShed, eventClaimAcquired, eventClaimReleased, eventClaimForceReleased, eventRunStarted, eventRunFinished, eventError:
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO scheduler_events (seq, type, workflow, run_id, reason, status, occurred_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(seq) DO NOTHING`,
				ev.Seq, ev.Type, nullIfEmpty(ev.Workflow), nullIfEmpty(ev.RunID), nullIfEmpty(ev.Reason), nullIfEmpty(ev.Status), formatTime(ev.Time)); err != nil {
				return fmt.Errorf("rollup: insert scheduler_event seq %d: %w", ev.Seq, err)
			}
			if ev.Type == eventError && ev.Error != nil {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO scheduler_errors (seq, code, error_class, message, occurred_at)
					VALUES (?, ?, ?, ?, ?)
					ON CONFLICT(seq) DO NOTHING`,
					ev.Seq, ev.Error.Code, nullIfEmpty(string(telemetry.ClassifyError(ev.Error.Code))),
					nullIfEmpty(capMessage(telemetry.Redact(ev.Error.Message))), formatTime(ev.Time)); err != nil {
					return fmt.Errorf("rollup: insert scheduler_error seq %d: %w", ev.Seq, err)
				}
			}
			if ev.Type == eventInitCompleted {
				if err := upsertTimeToFirstPR(ctx, tx, ev.Time, time.Time{}); err != nil {
					return err
				}
			}
		case eventWorkflowStarved:
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO scheduler_events (seq, type, workflow, run_id, reason, status, occurred_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(seq) DO NOTHING`,
				ev.Seq, ev.Type, nullIfEmpty(ev.Workflow), nullIfEmpty(ev.RunID), nullIfEmpty(ev.Reason), nullIfEmpty(ev.Status), formatTime(ev.Time)); err != nil {
				return fmt.Errorf("rollup: insert scheduler_event seq %d: %w", ev.Seq, err)
			}
		}
	}
	// Incremental replay applies here too: spans is now bounded to the tail
	// past spanCursor.byteOffset, not the whole rolling file (previously
	// re-parsed AND delete+reinserted in full every tick — O(all-spans-ever),
	// 75K spans / 55.6MB observed in the wild). The per-span delete-then-insert
	// stays: it is what makes replaying an already-ingested span (the
	// reset-from-head fallback in readSchedulerSpansFrom) harmless rather than
	// a duplicate-key error.
	for _, span := range spans {
		if span.TraceID == "" {
			return fmt.Errorf("rollup: scheduler span %s has no trace id", span.SpanID)
		}
		if err := deleteSpan(ctx, tx, span.TraceID, span.SpanID); err != nil {
			return err
		}
		if err := insertSpans(ctx, tx, span.TraceID, []telemetry.SpanRecord{span}); err != nil {
			return err
		}
	}
	if err := writeSchedulerCursor(ctx, tx, newOffset, maxSeq); err != nil {
		return err
	}
	if err := writeSpansCursor(ctx, tx, newSpanOffset); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rollup: commit scheduler ingest tx: %w", err)
	}
	// Checkpoint the WAL back into the main db file after every ingest
	// (#530 maintainer ruling). WAL mode already gives a separate reader
	// process (goobers telemetry/trace) correct read-after-commit
	// visibility without this — what an unchecked WAL actually risks is
	// unbounded file growth across thousands of incremental per-tick
	// ingests, and a non-WAL-aware tool (a raw copy of just the .db file)
	// missing not-yet-checkpointed rows. Best-effort only: this connection
	// is the sole writer (SetMaxOpenConns(1)), so nothing else can be
	// mid-write, but a concurrent reader transaction from another process
	// can legitimately hold the checkpoint back — that's a maintenance
	// delay, not a correctness problem, so its failure must never surface
	// as an ingest failure.
	checkpointWAL(db.sql)
	return nil
}

// MaintainSchedulerRetention serializes journal compaction and cursor reset
// with incremental ingestion. The cursor is durably reset before compact runs,
// so a crash after journal replacement cannot leave an old-file byte offset.
func (db *DB) MaintainSchedulerRetention(
	ctx context.Context,
	schedulerDir string,
	cutoff time.Time,
	compact func() error,
) error {
	db.schedulerMu.Lock()
	defer db.schedulerMu.Unlock()

	if err := db.ingestSchedulerLog(ctx, schedulerDir); err != nil {
		return fmt.Errorf("rollup: ingest scheduler journal before retention: %w", err)
	}
	if _, err := db.PruneSchedulerBefore(ctx, cutoff); err != nil {
		return err
	}
	if err := db.resetSchedulerIngestCursor(ctx); err != nil {
		return err
	}
	return compact()
}

func deleteSpan(ctx context.Context, tx *sql.Tx, runID, spanID string) error {
	for _, table := range []string{"span_events", "span_business_status", "agent_invocations"} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE run_id = ? AND span_id = ?`, table), runID, spanID); err != nil {
			return fmt.Errorf("rollup: clear %s for span %s: %w", table, spanID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM spans WHERE run_id = ? AND span_id = ?`, runID, spanID); err != nil {
		return fmt.Errorf("rollup: clear span %s: %w", spanID, err)
	}
	return nil
}

func insertSpans(ctx context.Context, tx *sql.Tx, runID string, spans []telemetry.SpanRecord) error {
	for _, s := range spans {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO spans (run_id, span_id, parent_span_id, name, kind, status, status_message, start_time, end_time, duration_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, s.SpanID, nullIfEmpty(s.ParentSpanID), s.Name, nullIfEmpty(s.Kind), s.Status,
			nullIfEmpty(s.StatusMessage), formatTime(s.StartTime), formatTime(s.EndTime), durationMillis(s.StartTime, s.EndTime)); err != nil {
			return fmt.Errorf("rollup: insert span %s: %w", s.SpanID, err)
		}
		// The canonical outcome rides the span's own generic Attributes
		// map (Span.Complete sets it as an OTel attribute; JournalSpanExporter
		// already captures every attribute into SpanRecord.Attributes with no
		// exporter change needed) — a satellite row, not a spans column (see
		// schema.go's v3 migration comment): empty/absent for a span
		// predating this fix or one that never called Complete (a gate span,
		// still Succeed/Fail).
		businessStatus := s.Attributes[telemetry.AttrOutcome]
		if businessStatus == "" {
			businessStatus = s.Attributes["goobers.business_status"]
		}
		if businessStatus != "" {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO span_business_status (run_id, span_id, business_status)
				VALUES (?, ?, ?)`,
				runID, s.SpanID, businessStatus); err != nil {
				return fmt.Errorf("rollup: insert span_business_status %s: %w", s.SpanID, err)
			}
		}
		if err := insertAgentInvocation(ctx, tx, runID, s); err != nil {
			return err
		}
		if err := insertStageUsage(ctx, tx, runID, s); err != nil {
			return err
		}
		for i, ev := range s.Events {
			attrsJSON, err := marshalAttributes(ev.Attributes)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO span_events (run_id, span_id, seq, name, occurred_at, attributes_json)
				VALUES (?, ?, ?, ?, ?, ?)`,
				runID, s.SpanID, i, ev.Name, formatTime(ev.Time), attrsJSON); err != nil {
				return fmt.Errorf("rollup: insert span_event %s/%d: %w", s.SpanID, i, err)
			}
		}
	}
	return nil
}

func insertAgentInvocation(ctx context.Context, tx *sql.Tx, runID string, span telemetry.SpanRecord) error {
	model, hasModel := span.Attributes[telemetry.AttrModel]
	harnessVersion, hasHarnessVersion := span.Attributes[telemetry.AttrHarnessVersion]
	if !hasModel && !hasHarnessVersion {
		return nil
	}
	if !hasModel || !hasHarnessVersion {
		return fmt.Errorf("rollup: agent span %s has incomplete model/harness version provenance", span.SpanID)
	}
	if span.Kind != telemetry.SpanKindTask && span.Kind != telemetry.SpanKindGate {
		return fmt.Errorf("rollup: span %s carries agent provenance but has kind %q", span.SpanID, span.Kind)
	}
	stage := span.Attributes[telemetry.AttrStage]
	if stage == "" {
		return fmt.Errorf("rollup: agent span %s has no %s attribute", span.SpanID, telemetry.AttrStage)
	}

	var traversal, attempt sql.NullInt64
	if span.Kind == telemetry.SpanKindTask {
		attemptNumber, err := strconv.Atoi(span.Attributes[telemetry.AttrAttemptNumber])
		if err != nil || attemptNumber < 1 {
			return fmt.Errorf("rollup: agent span %s has invalid %s attribute %q", span.SpanID, telemetry.AttrAttemptNumber, span.Attributes[telemetry.AttrAttemptNumber])
		}
		traversalNumber, matched, err := matchingTraversalForSpan(ctx, tx, runID, stage, attemptNumber, span)
		if err != nil {
			return err
		}
		if matched {
			traversal = sql.NullInt64{Int64: int64(traversalNumber), Valid: true}
		}
		attempt = sql.NullInt64{Int64: int64(attemptNumber), Valid: true}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_invocations (
			run_id, span_id, kind, stage, traversal, attempt, model, harness_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, span.SpanID, span.Kind, stage, traversal, attempt, model, harnessVersion); err != nil {
		return fmt.Errorf("rollup: insert agent invocation %s: %w", span.SpanID, err)
	}
	return nil
}

func insertStageUsage(ctx context.Context, tx *sql.Tx, runID string, span telemetry.SpanRecord) error {
	// Agentic gates use the same harness as tasks, so they also carry usage
	// attributes. Gates have no stage-attempt identity and are intentionally
	// outside the per-stage aggregates.
	if span.Kind == telemetry.SpanKindGate {
		return nil
	}
	stage := span.Attributes[telemetry.AttrStage]
	attemptText := span.Attributes[telemetry.AttrAttemptNumber]
	attempt, attemptErr := strconv.Atoi(attemptText)
	input, hasInput, err := usageInt64(span.SpanID, span.Attributes, telemetry.AttrGenAIUsageInputTokens)
	if err != nil {
		return err
	}
	output, hasOutput, err := usageInt64(span.SpanID, span.Attributes, telemetry.AttrGenAIUsageOutputTokens)
	if err != nil {
		return err
	}
	premium, hasPremium, err := usageFloat64(span.SpanID, span.Attributes, telemetry.AttrCopilotPremiumRequests)
	if err != nil {
		return err
	}
	cost, hasCost, err := usageFloat64(span.SpanID, span.Attributes, telemetry.AttrUsageCostUSD)
	if err != nil {
		return err
	}
	models, err := modelUsageFromSpan(span)
	if err != nil {
		return err
	}
	hasAggregate := hasInput || hasOutput || hasPremium || hasCost
	if len(models) == 0 && hasAggregate {
		if model := span.Attributes[telemetry.AttrGenAIResponseModel]; model != "" {
			models = append(models, modelUsageRecord{
				model:      model,
				input:      input,
				hasInput:   hasInput,
				output:     output,
				hasOutput:  hasOutput,
				premium:    premium,
				hasPremium: hasPremium,
				cost:       cost,
				hasCost:    hasCost,
			})
		}
	}
	if !hasAggregate && len(models) == 0 {
		return nil
	}
	if span.Kind != telemetry.SpanKindTask {
		return fmt.Errorf("rollup: span %s carries agent usage but has kind %q, want %q", span.SpanID, span.Kind, telemetry.SpanKindTask)
	}
	if stage == "" {
		return fmt.Errorf("rollup: usage span %s has no %s attribute", span.SpanID, telemetry.AttrStage)
	}
	if attemptErr != nil || attempt < 1 {
		return fmt.Errorf("rollup: usage span %s has invalid %s attribute %q", span.SpanID, telemetry.AttrAttemptNumber, attemptText)
	}
	traversal, err := traversalForUsageSpan(tx, runID, stage, attempt, span)
	if err != nil {
		return err
	}

	if hasAggregate {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO stage_usage (
				run_id, stage, traversal, attempt, input_tokens, output_tokens, copilot_premium_requests, cost_usd, branch
			)
			SELECT run_id, stage, traversal, attempt, ?, ?, ?, ?, branch
			FROM stage_attempts
			WHERE run_id = ? AND stage = ? AND traversal = ? AND attempt = ?`,
			nullableInt64(input, hasInput), nullableInt64(output, hasOutput),
			nullableFloat64(premium, hasPremium), nullableFloat64(cost, hasCost),
			runID, stage, traversal, attempt)
		if err != nil {
			return fmt.Errorf("rollup: insert usage for stage %s traversal %d: %w", stage, traversal, err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("rollup: inspect usage update for stage %s traversal %d: %w", stage, traversal, err)
		}
		if updated != 1 {
			return fmt.Errorf("rollup: usage span %s has no matching stage %s traversal %d", span.SpanID, stage, traversal)
		}
	}
	for _, model := range models {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO stage_model_usage (
				run_id, stage, traversal, attempt, model, input_tokens, output_tokens,
				copilot_premium_requests, cost_usd
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, stage, traversal, attempt, model.model,
			nullableInt64(model.input, model.hasInput),
			nullableInt64(model.output, model.hasOutput),
			nullableFloat64(model.premium, model.hasPremium),
			nullableFloat64(model.cost, model.hasCost)); err != nil {
			return fmt.Errorf("rollup: insert model usage for stage %s traversal %d model %s: %w", stage, traversal, model.model, err)
		}
	}
	return nil
}

type modelUsageRecord struct {
	model               string
	input, output       int64
	premium, cost       float64
	hasInput, hasOutput bool
	hasPremium, hasCost bool
}

func modelUsageFromSpan(span telemetry.SpanRecord) ([]modelUsageRecord, error) {
	var out []modelUsageRecord
	seen := make(map[string]struct{})
	for _, event := range span.Events {
		if event.Name != telemetry.GenAIModelUsageEventName {
			continue
		}
		model := event.Attributes[telemetry.AttrGenAIResponseModel]
		if model == "" {
			return nil, fmt.Errorf("rollup: span %s has model usage event without %s", span.SpanID, telemetry.AttrGenAIResponseModel)
		}
		if _, duplicate := seen[model]; duplicate {
			return nil, fmt.Errorf("rollup: span %s has duplicate model usage for %q", span.SpanID, model)
		}
		seen[model] = struct{}{}
		input, hasInput, err := usageInt64(span.SpanID, event.Attributes, telemetry.AttrGenAIUsageInputTokens)
		if err != nil {
			return nil, err
		}
		output, hasOutput, err := usageInt64(span.SpanID, event.Attributes, telemetry.AttrGenAIUsageOutputTokens)
		if err != nil {
			return nil, err
		}
		premium, hasPremium, err := usageFloat64(span.SpanID, event.Attributes, telemetry.AttrCopilotPremiumRequests)
		if err != nil {
			return nil, err
		}
		cost, hasCost, err := usageFloat64(span.SpanID, event.Attributes, telemetry.AttrUsageCostUSD)
		if err != nil {
			return nil, err
		}
		if !hasInput && !hasOutput && !hasPremium && !hasCost {
			return nil, fmt.Errorf("rollup: span %s has unmeasured model usage for %q", span.SpanID, model)
		}
		out = append(out, modelUsageRecord{
			model: model, input: input, hasInput: hasInput, output: output, hasOutput: hasOutput,
			premium: premium, hasPremium: hasPremium, cost: cost, hasCost: hasCost,
		})
	}
	return out, nil
}

func traversalForUsageSpan(tx *sql.Tx, runID, stage string, attempt int, span telemetry.SpanRecord) (int, error) {
	traversal, matched, err := matchingTraversalForSpan(context.Background(), tx, runID, stage, attempt, span)
	if err != nil {
		return 0, err
	}
	if !matched {
		return 0, fmt.Errorf("rollup: usage span %s has no matching stage attempt %s/%d", span.SpanID, stage, attempt)
	}
	return traversal, nil
}

func matchingTraversalForSpan(ctx context.Context, tx *sql.Tx, runID, stage string, attempt int, span telemetry.SpanRecord) (int, bool, error) {
	if span.StartTime.IsZero() || span.EndTime.IsZero() || span.EndTime.Before(span.StartTime) {
		return 0, false, fmt.Errorf("rollup: span %s has invalid time window", span.SpanID)
	}
	query := `
		SELECT traversal, started_at
		FROM stage_attempts
		WHERE run_id = ? AND stage = ? AND attempt = ?`
	args := []any{runID, stage, attempt}
	if rawBranch, ok := span.Attributes[telemetry.AttrBranch]; ok {
		branch, err := strconv.Atoi(rawBranch)
		if err != nil || branch < 0 {
			return 0, false, fmt.Errorf("rollup: span %s has invalid %s attribute %q", span.SpanID, telemetry.AttrBranch, rawBranch)
		}
		query += ` AND branch = ?`
		args = append(args, branch)
	}
	query += ` ORDER BY traversal`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, false, fmt.Errorf("rollup: query traversal for span %s: %w", span.SpanID, err)
	}
	defer func() { _ = rows.Close() }()

	traversal := 0
	for rows.Next() {
		var candidate int
		var startedAtText sql.NullString
		if err := rows.Scan(&candidate, &startedAtText); err != nil {
			return 0, false, fmt.Errorf("rollup: scan traversal for span %s: %w", span.SpanID, err)
		}
		startedAt, err := parseTime(startedAtText)
		if err != nil {
			return 0, false, err
		}
		if startedAt.Before(span.StartTime) || startedAt.After(span.EndTime) {
			continue
		}
		if traversal != 0 {
			return 0, false, fmt.Errorf("rollup: span %s matches multiple traversals for stage attempt %s/%d", span.SpanID, stage, attempt)
		}
		traversal = candidate
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("rollup: iterate traversals for span %s: %w", span.SpanID, err)
	}
	return traversal, traversal != 0, nil
}

func usageInt64(spanID string, attrs map[string]string, name string) (int64, bool, error) {
	raw, ok := attrs[name]
	if !ok {
		return 0, false, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, false, fmt.Errorf("rollup: span %s has invalid %s attribute %q", spanID, name, raw)
	}
	return value, true, nil
}

func usageFloat64(spanID string, attrs map[string]string, name string) (float64, bool, error) {
	raw, ok := attrs[name]
	if !ok {
		return 0, false, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false, fmt.Errorf("rollup: span %s has invalid %s attribute %q", spanID, name, raw)
	}
	return value, true, nil
}

func nullableInt64(value int64, valid bool) sql.NullInt64 {
	return sql.NullInt64{Int64: value, Valid: valid}
}

func nullableFloat64(value float64, valid bool) sql.NullFloat64 {
	return sql.NullFloat64{Float64: value, Valid: valid}
}
