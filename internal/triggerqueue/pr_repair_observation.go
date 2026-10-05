package triggerqueue

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/providers"
)

const prRepairObservationSchema = `
ALTER TABLE pr_repair_commands ADD COLUMN observation BLOB NOT NULL DEFAULT '' CHECK(length(observation)<=65536);
ALTER TABLE pr_repair_commands ADD COLUMN observation_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE pr_repair_commands ADD COLUMN observed_ns INTEGER;
`

// MaxPRRepairObservations bounds retained check history; omitted checks are counted.
const MaxPRRepairObservations = 16
const maxPRRepairObservationBytes = 64 << 10

type repairObservationHistory struct {
	Observations []sessioning.PRRepairObservation
	Omitted      int64
}

// ProvenCommit returns only acknowledged+verified or separately observed exact evidence.
func (record PRRepairCommand) ProvenCommit() string {
	if record.State == "confirmed" && record.Receipt != nil && record.Receipt.ProviderAcknowledged && record.Receipt.ObservedMatches {
		return record.Receipt.CommitID
	}
	if record.State == "observed-applied" && len(record.Observations) > 0 {
		return record.Observations[len(record.Observations)-1].CommitID
	}
	return ""
}

// PRRepairCommandForReview is a host-only gaggle lookup. Current source read and
// repair authorization must precede returning any data to its caller.
func (s *Store) PRRepairCommandForReview(ctx context.Context, gaggle, id string) (PRRepairCommand, error) {
	if !validChildText(gaggle, 253, true) || !validPRRepairID(id) {
		return PRRepairCommand{}, ErrTransition
	}
	r, err := scanPRRepairCommand(s.db.QueryRowContext(ctx, "SELECT "+prRepairColumns+" FROM pr_repair_commands WHERE gaggle=? AND id=?", gaggle, id))
	if err == nil && r.TombstonedAt != nil {
		err = ErrWorkbenchCommandExpired
	}
	return r, err
}

// ObservePRRepairCommand records an exact read after the provider attempt joined.
// It never changes the original acknowledgement, retries a write, or infers
// non-application from absence. A positive proof releases custody atomically.
func (s *Store) ObservePRRepairCommand(ctx context.Context, scope WorkbenchCommandScope, id, digest string, checker sessioning.Actor, result providers.PRRepairObservation, now time.Time) (PRRepairCommand, error) {
	if !validWorkbenchScope(scope) || !validPRRepairID(id) || !validWorkbenchDigest(digest) || !validChildText(checker.Issuer, 2048, true) || !validChildText(checker.Subject, 512, true) || now.IsZero() {
		return PRRepairCommand{}, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PRRepairCommand{}, err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := prRepairCommandTx(ctx, tx, scope, id)
	if err != nil {
		return r, err
	}
	if r.RequestDigest != digest {
		return r, ErrConflict
	}
	if r.State == "observed-applied" {
		return r, nil
	}
	if err = canObserveRepair(r, now); err != nil {
		return r, err
	}
	o := sessioning.PRRepairObservation{Checker: checker, At: now.UTC(), Matches: result.Matches, CommitID: result.CommitID}
	if err = validateRepairObservation(r, o); err != nil {
		return r, err
	}
	prior, _ := json.Marshal(repairObservationHistory{r.Observations, r.OmittedObservations})
	r.Observations = append(r.Observations, o)
	if len(r.Observations) > MaxPRRepairObservations {
		r.Observations = r.Observations[1:]
		if r.OmittedObservations < math.MaxInt64 {
			r.OmittedObservations++
		}
	}
	raw, err := json.Marshal(repairObservationHistory{r.Observations, r.OmittedObservations})
	if err != nil || len(raw) > maxPRRepairObservationBytes {
		return r, ErrTransition
	}
	if err = childByteCapacity(ctx, tx, len(raw)-len(prior)+8192); err != nil {
		return r, err
	}
	var observed any
	if o.Matches {
		observed = o.At.UnixNano()
	}
	res, err := tx.ExecContext(ctx, `UPDATE pr_repair_commands SET observation=?,observation_digest=?,observed_ns=? WHERE id=? AND state='unknown' AND observed_ns IS NULL AND request_digest=?`, raw, childDigest(raw), observed, id, digest)
	if err = changed(res, err); err != nil {
		return r, err
	}
	r, err = prRepairCommandTx(ctx, tx, scope, id)
	if err != nil {
		return r, err
	}
	return r, tx.Commit()
}

func canObserveRepair(r PRRepairCommand, now time.Time) error {
	if r.State != "unknown" || r.CompletedAt == nil || r.Receipt == nil || !r.Receipt.MutationAttempted || now.Before(*r.CompletedAt) {
		return ErrTransition
	}
	if len(r.Observations) > 0 && now.Before(r.Observations[len(r.Observations)-1].At) {
		return ErrTransition
	}
	return nil
}

func validateRepairObservation(r PRRepairCommand, o sessioning.PRRepairObservation) error {
	if !validChildText(o.Checker.Issuer, 2048, true) || !validChildText(o.Checker.Subject, 512, true) || o.At.IsZero() || r.CompletedAt == nil || o.At.Before(*r.CompletedAt) {
		return ErrTransition
	}
	if o.Matches {
		if !providers.ValidSourceCommit(o.CommitID) || o.CommitID == r.Input.Request.ExpectedHeadSHA || (r.Receipt != nil && r.Receipt.CommitID != "" && r.Receipt.CommitID != o.CommitID) {
			return ErrConflict
		}
	} else if o.CommitID != "" {
		return ErrConflict
	}
	return nil
}

func decodePRRepairObservation(r *PRRepairCommand, raw []byte, digest string, observed sql.NullInt64) error {
	if len(raw) == 0 {
		if digest != "" || observed.Valid {
			return ErrConflict
		}
		return nil
	}
	var h repairObservationHistory
	if len(raw) > maxPRRepairObservationBytes || childDigest(raw) != digest || json.Unmarshal(raw, &h) != nil || len(h.Observations) == 0 || len(h.Observations) > MaxPRRepairObservations || h.Omitted < 0 {
		return ErrConflict
	}
	canonical, _ := json.Marshal(h)
	if !bytes.Equal(canonical, raw) || r.State != "unknown" || r.Receipt == nil || !r.Receipt.MutationAttempted {
		return ErrConflict
	}
	var last time.Time
	for i, o := range h.Observations {
		if validateRepairObservation(*r, o) != nil || o.At.Before(last) || (o.Matches && i != len(h.Observations)-1) {
			return ErrConflict
		}
		last = o.At
	}
	tail := h.Observations[len(h.Observations)-1]
	if observed.Valid != tail.Matches || (observed.Valid && observed.Int64 != tail.At.UnixNano()) {
		return ErrConflict
	}
	r.Observations, r.OmittedObservations = h.Observations, h.Omitted
	if tail.Matches {
		r.State = "observed-applied"
	}
	return nil
}
