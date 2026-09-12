package rollup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// RecordedLandingIntent proves an attempt was persisted, not that it landed.
// Consumers must use Merges for verified totals, never count these attempts.
type RecordedLandingIntent struct {
	providers.LandingIntent
	Provider   string    `json:"provider"`
	InstanceID string    `json:"instanceId"`
	Gaggle     string    `json:"gaggle"`
	RunID      string    `json:"runId"`
	OccurredAt time.Time `json:"occurredAt"`
}

func (db *DB) readLandingIntents(ctx context.Context, query MergeReportQuery, report *MergeReport) error {
	rows, err := db.readDB().QueryContext(ctx, `SELECT i.provider, i.external_id, i.run_id, i.occurred_at,
	 r.gaggle, COALESCE(r.instance_id, ''),
	 CASE WHEN length(i.runner_json) <= 16384 THEN i.runner_json ELSE NULL END
	 FROM landing_intents i JOIN runs r ON r.run_id=i.run_id
	 WHERE i.occurred_at >= ? AND i.occurred_at < ?
	 ORDER BY i.occurred_at, i.run_id, i.seq LIMIT ?`, formatTime(query.Since), formatTime(query.Until), MaxMergeReportEvents-report.examinedEvents+1)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	report.LandingIntents = []RecordedLandingIntent{}
	for rows.Next() {
		report.examinedEvents++
		if report.examinedEvents > MaxMergeReportEvents {
			return fmt.Errorf("merge report exceeds %d retained events; narrow the query scope", MaxMergeReportEvents)
		}
		var entry RecordedLandingIntent
		var pullID string
		var timestamp, raw sql.NullString
		if err := rows.Scan(&entry.Provider, &pullID, &entry.RunID, &timestamp, &entry.Gaggle, &entry.InstanceID, &raw); err != nil {
			return err
		}
		if entry.OccurredAt, err = parseTime(timestamp); err != nil {
			return err
		}
		var fields struct {
			Intent *providers.LandingIntent `json:"landingIntent"`
		}
		if !raw.Valid || json.Unmarshal([]byte(raw.String), &fields) != nil || !validRecordedIntent(fields.Intent, pullID, entry.InstanceID, entry.Provider) {
			report.UnverifiedIntentEvents++
			continue
		}
		entry.LandingIntent = *fields.Intent
		if matchesMergeQuery(ConfirmedMerge{InstanceID: entry.InstanceID, Gaggle: entry.Gaggle, RepositoryAPIURL: entry.RepositoryAPIURL}, query) {
			report.LandingIntents = append(report.LandingIntents, entry)
		}
	}
	return rows.Err()
}

func validRecordedIntent(intent *providers.LandingIntent, pullID, instanceID, provider string) bool {
	return intent != nil && instance.ValidIdentity(intent.ID) && instance.ValidIdentity(instanceID) &&
		(provider == "github" || provider == "ado" || provider == "gitea") &&
		(intent.Operation == "merge" || intent.Operation == "enqueue") &&
		intent.PullID == pullID && canonicalMergePullID(pullID) && canonicalMergeRepository(intent.RepositoryAPIURL) && len(intent.ExpectedHeadSHA) <= 128
}
