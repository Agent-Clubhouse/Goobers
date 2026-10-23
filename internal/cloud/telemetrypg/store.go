// Package telemetrypg implements the deliberately minimal cloud telemetry port.
package telemetrypg

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
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
					error_class,signature
				) DO UPDATE
				SET observed_at=excluded.observed_at,finding=excluded.finding,
				    source_stream=excluded.source_stream,
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
		telemetryclient.AggregateGateNoise,
		telemetryclient.AggregateCreditAssignment:
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
	return nil
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
	rows, err := s.pool.Query(ctx, `
		SELECT aggregate,finding
		FROM telemetry.defect_aggregate
		WHERE instance_id=$1 AND gaggle=$2
		  AND ($3='' OR workflow=$3)
		  AND observed_at>=$4
		ORDER BY aggregate,kind,subject,error_class,signature`,
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
	for rows.Next() {
		var aggregate string
		var raw []byte
		if err := rows.Scan(&aggregate, &raw); err != nil {
			return response, err
		}
		if !allowed[aggregate] {
			continue
		}
		var finding telemetryclient.Finding
		if err := json.Unmarshal(raw, &finding); err != nil {
			return response, err
		}
		if findingPassesThresholds(finding, request.Thresholds) {
			response.Findings = append(response.Findings, finding)
		}
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

func findingPassesThresholds(
	finding telemetryclient.Finding,
	thresholds telemetryclient.Thresholds,
) bool {
	switch finding.Kind {
	case "stage-failure-rate":
		return (thresholds.MinSamples == 0 ||
			finding.Metrics["totalAttempts"] >= float64(thresholds.MinSamples)) &&
			(thresholds.MaxFailureRate == 0 ||
				finding.Metrics["failureRate"] >= thresholds.MaxFailureRate)
	case "error-signature":
		return thresholds.MinErrorSignatureCount == 0 ||
			finding.Metrics["count"] >= float64(thresholds.MinErrorSignatureCount)
	case "gate-never-fails":
		return thresholds.MinGateEvaluations == 0 ||
			finding.Metrics["totalEvaluations"] >= float64(thresholds.MinGateEvaluations)
	case "gate-repass-churn":
		return (thresholds.MinGateEvaluations == 0 ||
			finding.Metrics["totalEvaluations"] >= float64(thresholds.MinGateEvaluations)) &&
			(thresholds.MaxGateEscalationRate == 0 ||
				finding.Metrics["escalationRate"] >= thresholds.MaxGateEscalationRate)
	case "credit-assignment":
		return (thresholds.MinCreditRuns == 0 ||
			finding.Metrics["runs"] >= float64(thresholds.MinCreditRuns)) &&
			(thresholds.MinCreditFailureShare == 0 ||
				finding.Metrics["failureShare"] >= thresholds.MinCreditFailureShare)
	default:
		return false
	}
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
