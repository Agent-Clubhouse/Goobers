// Package telemetrypg implements the deliberately minimal cloud telemetry port.
package telemetrypg

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/gatenoise"
	"github.com/goobers/goobers/internal/telemetryclient"
)

const maxFindingBytes = 64 << 10

// Store is one instance's minimal telemetry projection.
type Store struct {
	pool       *pgxpool.Pool
	instanceID string
	manager    *cloudlease.Manager
	handle     *cloudlease.Handle
}

// New constructs an instance-scoped telemetry store.
func New(pool *pgxpool.Pool, instanceID string) (*Store, error) {
	if pool == nil || strings.TrimSpace(instanceID) == "" {
		return nil, errors.New("telemetrypg: pool and instance are required")
	}
	return &Store{pool: pool, instanceID: instanceID}, nil
}

// BindFence supplies the live controller lease used by projection writes.
func (s *Store) BindFence(manager *cloudlease.Manager, handle *cloudlease.Handle) {
	s.manager, s.handle = manager, handle
}

// AggregateRow is one materialized admitted finding at a source position.
type AggregateRow struct {
	Gaggle     string
	Workflow   string
	Aggregate  telemetryclient.Aggregate
	ObservedAt time.Time
	Finding    telemetryclient.Finding
}

// Milestones carries monotonic first-success observations.
type Milestones struct {
	InitCompletedAt time.Time
	FirstPROpenAt   time.Time
}

// Batch applies materialized telemetry and its stream cursor atomically.
type Batch struct {
	Consumer   string
	Current    journal.StreamCursor
	NextSeq    uint64
	Milestones Milestones
	Aggregates []AggregateRow
	// Gates are this source's COMPLETE per-gate evaluation totals, including
	// gates below every threshold. Gate-noise findings are derived from them at
	// read time (gate-noise cannot be sent as Aggregates).
	Gates []GateRow
}

// GateRow is one gate's complete evaluation counts at the batch's source position.
type GateRow struct {
	Gaggle        string
	Workflow      string
	Gate          string
	Total         int64
	NonPass       int64
	Escalated     int64
	ObservedAt    time.Time
	EvaluatedRuns []telemetryclient.JournalPointer
	EscalatedRuns []telemetryclient.JournalPointer
}

// Apply commits one exact stream prefix under the live fence.
func (s *Store) Apply(ctx context.Context, batch Batch) error {
	if batch.Consumer == "" || batch.Current.Stream == "" ||
		batch.NextSeq <= batch.Current.Seq || batch.NextSeq > 1<<63-1 ||
		batch.Current.Seq > 1<<63-1 {
		return errors.New("telemetrypg: invalid projection batch")
	}
	batch.Aggregates = append([]AggregateRow(nil), batch.Aggregates...)
	for index := range batch.Aggregates {
		if err := normalizeAggregate(&batch.Aggregates[index]); err != nil {
			return err
		}
	}
	batch.Aggregates = collapseAggregates(batch.Aggregates)
	batch.Milestones.InitCompletedAt = normalizeTimestamp(batch.Milestones.InitCompletedAt)
	batch.Milestones.FirstPROpenAt = normalizeTimestamp(batch.Milestones.FirstPROpenAt)
	gates, err := normalizeGates(batch.Gates)
	if err != nil {
		return err
	}
	batch.Gates = gates
	fingerprint, err := telemetryBatchFingerprint(batch)
	if err != nil {
		return err
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		if _, err := tx.Exec(txCtx, `
			SELECT pg_advisory_xact_lock(hashtextextended(
				'telemetry:'||$1||':'||$2||':'||$3,0))`,
			s.instanceID, batch.Consumer, batch.Current.Stream,
		); err != nil {
			return err
		}
		replay, err := s.checkApplyCursor(
			txCtx, tx, batch.Consumer, batch.Current, batch.NextSeq,
			fingerprint, epoch)
		if err != nil {
			return err
		}
		if replay {
			return nil
		}
		if err := s.applyMilestones(txCtx, tx, batch.Milestones, epoch); err != nil {
			return err
		}
		for _, aggregate := range batch.Aggregates {
			raw, err := json.Marshal(aggregate.Finding)
			if err != nil || len(raw) > maxFindingBytes {
				return errors.New("telemetrypg: finding exceeds storage bound")
			}
			tag, err := tx.Exec(txCtx, `
				INSERT INTO telemetry.defect_aggregate (
					instance_id,gaggle,workflow,aggregate,kind,subject,
					error_class,signature,observed_at,finding,
					source_stream,source_seq,owner_epoch
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
				ON CONFLICT (
					instance_id,gaggle,workflow,aggregate,kind,subject,
					error_class,signature,source_stream
				) DO UPDATE
				SET observed_at=excluded.observed_at,finding=excluded.finding,
				    source_seq=excluded.source_seq,owner_epoch=excluded.owner_epoch
				WHERE telemetry.defect_aggregate.instance_id=$1
				  AND telemetry.defect_aggregate.owner_epoch<=$13
				  AND telemetry.defect_aggregate.source_seq<excluded.source_seq`,
				s.instanceID, aggregate.Gaggle, aggregate.Workflow,
				aggregate.Aggregate, aggregate.Finding.Kind,
				aggregate.Finding.Subject, aggregate.Finding.ErrorClass,
				aggregate.Finding.Signature, aggregate.ObservedAt.UTC(), raw,
				batch.Current.Stream, int64(batch.NextSeq), epoch,
			)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return journal.ErrStreamCursorConflict
			}
		}
		for _, gate := range batch.Gates {
			if err := s.applyGate(txCtx, tx, gate, batch.Current.Stream, int64(batch.NextSeq), epoch); err != nil {
				return err
			}
		}
		return s.advanceCursor(
			txCtx, tx, batch.Consumer, batch.Current, batch.NextSeq,
			fingerprint, epoch)
	})
}

func telemetryBatchFingerprint(batch Batch) (string, error) {
	raw, err := json.Marshal(batch)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func (s *Store) checkApplyCursor(
	ctx context.Context,
	tx pgx.Tx,
	consumer string,
	cursor journal.StreamCursor,
	next uint64,
	fingerprint string,
	epoch int64,
) (bool, error) {
	var seq, ownerEpoch int64
	var storedFingerprint string
	err := tx.QueryRow(ctx, `
		SELECT seq,fingerprint,owner_epoch FROM telemetry.consumer_cursor
		WHERE instance_id=$1 AND consumer=$2 AND stream=$3
		FOR UPDATE`,
		s.instanceID, consumer, cursor.Stream,
	).Scan(&seq, &storedFingerprint, &ownerEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		if cursor.Seq != 0 {
			return false, journal.ErrStreamCursorConflict
		}
		return false, nil
	}
	if err != nil || ownerEpoch > epoch {
		return false, journal.ErrStreamCursorConflict
	}
	if uint64(seq) == next {
		if storedFingerprint != fingerprint {
			return false, journal.ErrStreamCursorConflict
		}
		return true, nil
	}
	if uint64(seq) != cursor.Seq {
		return false, journal.ErrStreamCursorConflict
	}
	return false, nil
}

func normalizeAggregate(row *AggregateRow) error {
	switch row.Aggregate {
	case telemetryclient.AggregateStageFailureRate,
		telemetryclient.AggregateCreditAssignment:
	case telemetryclient.AggregateGateNoise:
		return errors.New("telemetrypg: gate-noise is derived from Batch.Gates totals, not materialized findings")
	case telemetryclient.AggregateErrorSignature:
		subject, signature := telemetryclient.NormalizeErrorSignature(
			row.Finding.Subject)
		row.Finding.Subject, row.Finding.Signature = subject, signature
	default:
		return fmt.Errorf("telemetrypg: aggregate %q is not available in cloud",
			row.Aggregate)
	}
	if row.Gaggle == "" || row.ObservedAt.IsZero() ||
		row.Finding.Kind == "" || row.Finding.Subject == "" {
		return errors.New("telemetrypg: invalid aggregate row")
	}
	row.ObservedAt = normalizeTimestamp(row.ObservedAt)
	return nil
}

type aggregateKey struct {
	gaggle     string
	workflow   string
	aggregate  telemetryclient.Aggregate
	kind       string
	subject    string
	errorClass string
	signature  string
}

func collapseAggregates(rows []AggregateRow) []AggregateRow {
	collapsed := make([]AggregateRow, 0, len(rows))
	indexByKey := make(map[aggregateKey]int, len(rows))
	for _, row := range rows {
		key := aggregateKey{
			gaggle: row.Gaggle, workflow: row.Workflow,
			aggregate: row.Aggregate, kind: row.Finding.Kind,
			subject: row.Finding.Subject, errorClass: row.Finding.ErrorClass,
			signature: row.Finding.Signature,
		}
		if index, ok := indexByKey[key]; ok {
			collapsed[index] = row
			continue
		}
		indexByKey[key] = len(collapsed)
		collapsed = append(collapsed, row)
	}
	return collapsed
}

func normalizeTimestamp(value time.Time) time.Time {
	if value.IsZero() {
		return value
	}
	return value.UTC().Truncate(time.Microsecond)
}

func (s *Store) checkCursor(
	ctx context.Context,
	tx pgx.Tx,
	consumer string,
	cursor journal.StreamCursor,
	epoch int64,
) error {
	var seq, ownerEpoch int64
	err := tx.QueryRow(ctx, `
		SELECT seq,owner_epoch FROM telemetry.consumer_cursor
		WHERE instance_id=$1 AND consumer=$2 AND stream=$3
		FOR UPDATE`,
		s.instanceID, consumer, cursor.Stream,
	).Scan(&seq, &ownerEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		if cursor.Seq != 0 {
			return journal.ErrStreamCursorConflict
		}
		return nil
	}
	if err != nil || ownerEpoch > epoch || uint64(seq) != cursor.Seq {
		return journal.ErrStreamCursorConflict
	}
	return nil
}

func (s *Store) advanceCursor(
	ctx context.Context,
	tx pgx.Tx,
	consumer string,
	cursor journal.StreamCursor,
	next uint64,
	fingerprint string,
	epoch int64,
) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO telemetry.consumer_cursor (
			instance_id,consumer,stream,seq,fingerprint,owner_epoch
		) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (instance_id,consumer,stream) DO UPDATE
		SET seq=excluded.seq,fingerprint=excluded.fingerprint,
		    owner_epoch=excluded.owner_epoch,
		    updated_at=clock_timestamp()
		WHERE telemetry.consumer_cursor.instance_id=$1
		  AND telemetry.consumer_cursor.seq=$7
		  AND telemetry.consumer_cursor.seq<=excluded.seq
		  AND telemetry.consumer_cursor.owner_epoch<=$6`,
		s.instanceID, consumer, cursor.Stream, int64(next), fingerprint,
		epoch, int64(cursor.Seq),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return journal.ErrStreamCursorConflict
	}
	return nil
}

func (s *Store) applyMilestones(
	ctx context.Context,
	tx pgx.Tx,
	milestones Milestones,
	epoch int64,
) error {
	if milestones.InitCompletedAt.IsZero() && milestones.FirstPROpenAt.IsZero() {
		return nil
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO telemetry.first_success_milestones (
			instance_id,init_completed_at,first_pr_open_at,owner_epoch
		) VALUES ($1,$2,$3,$4)
		ON CONFLICT (instance_id) DO UPDATE
		SET init_completed_at=CASE
		      WHEN telemetry.first_success_milestones.init_completed_at IS NULL
		      THEN excluded.init_completed_at
		      WHEN excluded.init_completed_at IS NULL
		      THEN telemetry.first_success_milestones.init_completed_at
		      ELSE LEAST(telemetry.first_success_milestones.init_completed_at,
		                 excluded.init_completed_at) END,
		    first_pr_open_at=CASE
		      WHEN telemetry.first_success_milestones.first_pr_open_at IS NULL
		      THEN excluded.first_pr_open_at
		      WHEN excluded.first_pr_open_at IS NULL
		      THEN telemetry.first_success_milestones.first_pr_open_at
		      ELSE LEAST(telemetry.first_success_milestones.first_pr_open_at,
		                 excluded.first_pr_open_at) END,
		    owner_epoch=excluded.owner_epoch,updated_at=clock_timestamp()
		WHERE telemetry.first_success_milestones.instance_id=$1
		  AND telemetry.first_success_milestones.owner_epoch<=$4`,
		s.instanceID, nullableTime(milestones.InitCompletedAt),
		nullableTime(milestones.FirstPROpenAt), epoch,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return cloudlease.ErrStale
	}
	return nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}

// LoadStreamCursor implements the shared (stream, seq) consumer seam.
func (s *Store) LoadStreamCursor(
	ctx context.Context,
	consumer string,
	stream string,
) (journal.StreamCursor, error) {
	cursor := journal.StreamCursor{Stream: stream}
	err := s.pool.QueryRow(ctx, `
		SELECT seq FROM telemetry.consumer_cursor
		WHERE instance_id=$1 AND consumer=$2 AND stream=$3`,
		s.instanceID, consumer, stream,
	).Scan(&cursor.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return cursor, nil
	}
	return cursor, err
}

// AdvanceStreamCursor advances an empty batch under the same fenced CAS.
func (s *Store) AdvanceStreamCursor(
	ctx context.Context,
	consumer string,
	cursor journal.StreamCursor,
	next uint64,
) error {
	if next < cursor.Seq {
		return journal.ErrStreamCursorConflict
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		if err := s.checkCursor(txCtx, tx, consumer, cursor, epoch); err != nil {
			return err
		}
		if next == cursor.Seq {
			// A no-op advance changes nothing, and must not clear the committed
			// batch's fingerprint: that receipt is how an exact Apply replay
			// (a lost response) is recognized.
			return nil
		}
		return s.advanceCursor(txCtx, tx, consumer, cursor, next, "", epoch)
	})
}

// TimeToFirstPR returns the retained first-success metric.
func (s *Store) TimeToFirstPR(
	ctx context.Context,
) (telemetry.TimeToFirstPRMetric, error) {
	var initAt, firstPRAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT init_completed_at,first_pr_open_at
		FROM telemetry.first_success_milestones WHERE instance_id=$1`,
		s.instanceID,
	).Scan(&initAt, &firstPRAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return telemetry.TimeToFirstPRMetric{}, nil
	}
	if err != nil {
		return telemetry.TimeToFirstPRMetric{}, err
	}
	var initValue, firstPRValue time.Time
	if initAt != nil {
		initValue = initAt.UTC()
	}
	if firstPRAt != nil {
		firstPRValue = firstPRAt.UTC()
	}
	return telemetry.NewTimeToFirstPRMetric(initValue, firstPRValue), nil
}

// DefectAggregates implements the existing telemetry plane service.
func (s *Store) DefectAggregates(
	ctx context.Context,
	request httpapi.TelemetryDefectAggregateRequest,
) (telemetryclient.DefectAggregateResponse, error) {
	allowed := make(map[string]bool, len(request.Aggregates))
	for _, aggregate := range request.Aggregates {
		switch aggregate {
		case telemetryclient.AggregateStageFailureRate,
			telemetryclient.AggregateGateNoise,
			telemetryclient.AggregateCreditAssignment,
			telemetryclient.AggregateErrorSignature:
			allowed[string(aggregate)] = true
		default:
			return telemetryclient.DefectAggregateResponse{},
				fmt.Errorf("telemetrypg: aggregate %q is unavailable in cloud", aggregate)
		}
	}
	// Each source stream contributes its own row per aggregate identity, with a
	// watermark in that stream's own sequence domain. The read COMBINES them the
	// way the local rollup aggregates across its sources (mergeContributions),
	// so thresholds are applied to the combined counts, never to one source's.
	rows, err := s.pool.Query(ctx, `
		SELECT gaggle,workflow,aggregate,kind,subject,error_class,signature,finding
		FROM telemetry.defect_aggregate
		WHERE instance_id=$1 AND gaggle=$2
		  AND ($3='' OR workflow=$3)
		  AND observed_at>=$4
		ORDER BY gaggle,workflow,aggregate,kind,subject,error_class,signature,
		         observed_at DESC,source_stream ASC`,
		s.instanceID, request.Gaggle, request.Workflow, request.Since.UTC(),
	)
	if err != nil {
		return telemetryclient.DefectAggregateResponse{}, err
	}
	defer rows.Close()
	response := telemetryclient.DefectAggregateResponse{
		Gaggle: request.Gaggle, Workflow: request.Workflow, Since: request.Since,
		Findings:            []telemetryclient.Finding{},
		PromotionCandidates: []telemetryclient.PromotionSignal{},
	}
	maxFlagged := request.Thresholds.MaxFlaggedRuns
	if maxFlagged <= 0 {
		maxFlagged = defaultMaxFlaggedRuns
	}
	var group []telemetryclient.Finding
	var groupKey aggregateKey
	var groupAggregate string
	flush := func() {
		if len(group) == 0 {
			return
		}
		finding := mergeContributions(group, maxFlagged)
		if allowed[groupAggregate] && telemetryclient.FindingPassesThresholds(finding, request.Thresholds) {
			response.Findings = append(response.Findings, finding)
		}
		group = group[:0]
	}
	for rows.Next() {
		var gaggle, workflow, aggregate, kind, subject, errorClass, signature string
		var raw []byte
		if err := rows.Scan(&gaggle, &workflow, &aggregate, &kind, &subject, &errorClass, &signature, &raw); err != nil {
			return response, err
		}
		key := aggregateKey{gaggle: gaggle, workflow: workflow, aggregate: telemetryclient.Aggregate(aggregate),
			kind: kind, subject: subject, errorClass: errorClass, signature: signature}
		if len(group) > 0 && key != groupKey {
			flush()
		}
		groupKey, groupAggregate = key, aggregate
		var finding telemetryclient.Finding
		if err := json.Unmarshal(raw, &finding); err != nil {
			return response, err
		}
		group = append(group, finding)
	}
	if err := rows.Err(); err != nil {
		return response, err
	}
	flush()
	if allowed[string(telemetryclient.AggregateGateNoise)] {
		gateFindings, err := s.gateNoiseFindings(ctx, request)
		if err != nil {
			return response, err
		}
		response.Findings = append(response.Findings, gateFindings...)
	}
	sort.Slice(response.Findings, func(i, j int) bool {
		left, right := response.Findings[i], response.Findings[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Subject < right.Subject
	})
	if len(response.Findings) > telemetryclient.MaxFindings {
		response.Findings = response.Findings[:telemetryclient.MaxFindings]
		response.Truncated = true
	}
	for aggregate := range allowed {
		response.Aggregates = append(response.Aggregates, aggregate)
	}
	sort.Strings(response.Aggregates)
	return response, rows.Err()
}

func (s *Store) withFence(
	ctx context.Context,
	action func(context.Context, pgx.Tx, int64) error,
) error {
	if s.manager == nil || s.handle == nil ||
		s.handle.InstanceID != s.instanceID {
		return cloudlease.ErrStale
	}
	return s.manager.WithFence(ctx, s.handle,
		func(txCtx context.Context, tx pgx.Tx) error {
			return action(txCtx, tx, s.handle.Epoch)
		})
}

var _ journal.StreamCursorStore = (*Store)(nil)
var _ httpapi.TelemetryDefectAggregateService = (*Store)(nil)

// defaultMaxFlaggedRuns is the local rollup's default bound on the example runs
// one finding carries (rollup.Thresholds.MaxFlaggedRuns).
const defaultMaxFlaggedRuns = 10

// mergeContributions combines the per-source-stream contributions to one
// aggregate identity, newest observation first, as the local rollup combines its
// sources: counts add and every derived rate is recomputed from the summed
// counts (rollup.detectStageFailureRate, detectErrorSignatures, detectGateNoise).
//
//   - stage-failure-rate: totalAttempts and failedAttempts add; failureRate =
//     failed/total.
//   - error-signature: count adds.
//   - gate-noise is NOT merged here: its two kinds depend on each other's
//     counts, so it is derived from summed per-source Batch.Gates totals
//     (gateNoiseFindings).
//   - credit-assignment (and any other kind): a causal estimate is not a sum of
//     partial estimates, so the newest contribution stands.
//
// FlaggedRuns are the contributions' pointers, newest contribution first,
// de-duplicated and bounded.
func mergeContributions(contributions []telemetryclient.Finding, maxFlagged int) telemetryclient.Finding {
	merged := contributions[0]
	merged.Metrics = maps.Clone(contributions[0].Metrics)
	if merged.Metrics == nil {
		merged.Metrics = map[string]float64{}
	}
	if len(contributions) > 1 {
		sum := func(metric string) float64 {
			var total float64
			for _, contribution := range contributions {
				total += contribution.Metrics[metric]
			}
			return total
		}
		switch merged.Kind {
		case "stage-failure-rate":
			total, failed := sum("totalAttempts"), sum("failedAttempts")
			merged.Metrics["totalAttempts"], merged.Metrics["failedAttempts"] = total, failed
			if total > 0 {
				merged.Metrics["failureRate"] = failed / total
			}
		case "error-signature":
			merged.Metrics["count"] = sum("count")
		}
	}
	seen := map[telemetryclient.JournalPointer]bool{}
	var flagged []telemetryclient.JournalPointer
	for _, contribution := range contributions {
		for _, pointer := range contribution.FlaggedRuns {
			if len(flagged) >= maxFlagged {
				break
			}
			if !seen[pointer] {
				seen[pointer] = true
				flagged = append(flagged, pointer)
			}
		}
	}
	if len(flagged) > 0 {
		merged.FlaggedRuns = flagged
	}
	return merged
}

func normalizeGates(rows []GateRow) ([]GateRow, error) {
	out := append([]GateRow(nil), rows...)
	seen := map[[3]string]bool{}
	for index := range out {
		row := &out[index]
		if row.Gaggle == "" || row.Gate == "" || row.ObservedAt.IsZero() ||
			row.Total < 0 || row.NonPass < 0 || row.Escalated < 0 ||
			row.NonPass > row.Total || row.Escalated > row.Total {
			return nil, errors.New("telemetrypg: invalid gate totals")
		}
		key := [3]string{row.Gaggle, row.Workflow, row.Gate}
		if seen[key] {
			return nil, errors.New("telemetrypg: duplicate gate totals in one batch")
		}
		seen[key] = true
		row.ObservedAt = normalizeTimestamp(row.ObservedAt)
		if row.EvaluatedRuns == nil {
			row.EvaluatedRuns = []telemetryclient.JournalPointer{}
		}
		if row.EscalatedRuns == nil {
			row.EscalatedRuns = []telemetryclient.JournalPointer{}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Gaggle != b.Gaggle {
			return a.Gaggle < b.Gaggle
		}
		if a.Workflow != b.Workflow {
			return a.Workflow < b.Workflow
		}
		return a.Gate < b.Gate
	})
	return out, nil
}

// applyGate upserts one source stream's complete totals for a gate. The
// watermark compares source_seq only within that stream's own sequence domain.
func (s *Store) applyGate(ctx context.Context, tx pgx.Tx, gate GateRow, stream string, seq, epoch int64) error {
	evaluated, err := json.Marshal(gate.EvaluatedRuns)
	if err != nil {
		return err
	}
	escalated, err := json.Marshal(gate.EscalatedRuns)
	if err != nil {
		return err
	}
	if len(evaluated) > maxFindingBytes || len(escalated) > maxFindingBytes {
		return errors.New("telemetrypg: gate run evidence exceeds storage bound")
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO telemetry.gate_total (
			instance_id,gaggle,workflow,gate,source_stream,total,non_pass,
			escalated,evaluated_runs,escalated_runs,observed_at,source_seq,owner_epoch
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (instance_id,gaggle,workflow,gate,source_stream) DO UPDATE
		SET total=excluded.total,non_pass=excluded.non_pass,escalated=excluded.escalated,
		    evaluated_runs=excluded.evaluated_runs,escalated_runs=excluded.escalated_runs,
		    observed_at=excluded.observed_at,source_seq=excluded.source_seq,
		    owner_epoch=excluded.owner_epoch
		WHERE telemetry.gate_total.instance_id=$1
		  AND telemetry.gate_total.owner_epoch<=$13
		  AND telemetry.gate_total.source_seq<excluded.source_seq`,
		s.instanceID, gate.Gaggle, gate.Workflow, gate.Gate, stream,
		gate.Total, gate.NonPass, gate.Escalated, evaluated, escalated,
		gate.ObservedAt.UTC(), seq, epoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return journal.ErrStreamCursorConflict
	}
	return nil
}

// gateNoiseFindings derives both gate-noise kinds from the summed per-source
// totals with the SAME decision the local rollup uses (gatenoise.Evaluate), so
// a source that produced no finding of its own still contributes its
// denominator and its non-pass evidence.
func (s *Store) gateNoiseFindings(ctx context.Context, request httpapi.TelemetryDefectAggregateRequest) ([]telemetryclient.Finding, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT workflow,gate,total,non_pass,escalated,evaluated_runs,escalated_runs
		FROM telemetry.gate_total
		WHERE instance_id=$1 AND gaggle=$2 AND ($3='' OR workflow=$3)
		  AND observed_at>=$4
		ORDER BY workflow,gate,observed_at DESC,source_stream ASC`,
		s.instanceID, request.Gaggle, request.Workflow, request.Since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	thresholds := request.Thresholds
	minEvaluations, maxEscalation := thresholds.MinGateEvaluations, thresholds.MaxGateEscalationRate
	if thresholds == (telemetryclient.Thresholds{}) { // an unset request means the local defaults
		minEvaluations, maxEscalation = 5, 0.2
	}
	maxFlagged := thresholds.MaxFlaggedRuns
	if maxFlagged <= 0 {
		maxFlagged = defaultMaxFlaggedRuns
	}
	type gateGroup struct {
		totals    gatenoise.Totals
		evaluated []telemetryclient.JournalPointer
		escalated []telemetryclient.JournalPointer
	}
	groups := map[[2]string]*gateGroup{}
	var order [][2]string
	for rows.Next() {
		var workflow, gate string
		var total, nonPass, escalated int64
		var evaluatedRaw, escalatedRaw []byte
		if err := rows.Scan(&workflow, &gate, &total, &nonPass, &escalated, &evaluatedRaw, &escalatedRaw); err != nil {
			return nil, err
		}
		key := [2]string{workflow, gate}
		group := groups[key]
		if group == nil {
			group = &gateGroup{}
			groups[key] = group
			order = append(order, key)
		}
		group.totals = group.totals.Add(gatenoise.Totals{Total: total, NonPass: nonPass, Escalated: escalated})
		var evaluated, escalatedRuns []telemetryclient.JournalPointer
		if err := json.Unmarshal(evaluatedRaw, &evaluated); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(escalatedRaw, &escalatedRuns); err != nil {
			return nil, err
		}
		group.evaluated = append(group.evaluated, evaluated...)
		group.escalated = append(group.escalated, escalatedRuns...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var findings []telemetryclient.Finding
	for _, key := range order {
		group := groups[key]
		verdict := gatenoise.Evaluate(group.totals, minEvaluations, maxEscalation)
		if verdict.NeverFails {
			findings = append(findings, telemetryclient.Finding{
				Kind: "gate-never-fails", Subject: key[1],
				Metrics:     map[string]float64{"totalEvaluations": float64(group.totals.Total)},
				Threshold:   float64(minEvaluations),
				FlaggedRuns: boundedPointers(group.evaluated, maxFlagged),
			})
		}
		if verdict.Churn {
			findings = append(findings, telemetryclient.Finding{
				Kind: "gate-repass-churn", Subject: key[1],
				Metrics: map[string]float64{
					"escalationRate":   verdict.EscalationRate,
					"totalEvaluations": float64(group.totals.Total),
					"escalatedCount":   float64(group.totals.Escalated),
				},
				Threshold:   maxEscalation,
				FlaggedRuns: boundedPointers(group.escalated, maxFlagged),
			})
		}
	}
	return findings, nil
}

// boundedPointers de-duplicates (first occurrence wins, so the newest source
// comes first) and bounds example run pointers.
func boundedPointers(pointers []telemetryclient.JournalPointer, limit int) []telemetryclient.JournalPointer {
	seen := map[telemetryclient.JournalPointer]bool{}
	out := []telemetryclient.JournalPointer{}
	for _, pointer := range pointers {
		if len(out) >= limit {
			break
		}
		if !seen[pointer] {
			seen[pointer] = true
			out = append(out, pointer)
		}
	}
	return out
}
