package rollup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// AcceptedQueueEntry attributes an accepted enqueue, never a completed merge.
type AcceptedQueueEntry struct {
	providers.QueueAdmission
	Provider   string    `json:"provider"`
	InstanceID string    `json:"instanceId"`
	Gaggle     string    `json:"gaggle"`
	RunID      string    `json:"runId"`
	OccurredAt time.Time `json:"occurredAt"`
}

type queueEntryKey struct{ provider, repository, entry string }

func (db *DB) readQueueAdmissions(ctx context.Context, query MergeReportQuery, report *MergeReport) error {
	remaining := MaxMergeReportEvents - report.examinedEvents
	rows, err := db.readDB().QueryContext(ctx, `SELECT m.provider, m.external_id, m.run_id, m.occurred_at,
		r.gaggle, COALESCE(r.instance_id, ''),
		CASE WHEN length(m.runner_json) <= 16384 THEN m.runner_json ELSE NULL END
		FROM provider_mutations m JOIN runs r ON r.run_id = m.run_id
		WHERE m.kind = 'pr' AND m.operation = 'enqueue' AND m.occurred_at >= ? AND m.occurred_at < ?
		ORDER BY m.occurred_at, m.run_id, m.seq LIMIT ?`, formatTime(query.Since), formatTime(query.Until), remaining+1)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	entries := map[queueEntryKey]AcceptedQueueEntry{}
	conflicts := map[queueEntryKey]bool{}
	for rows.Next() {
		report.examinedEvents++
		remaining--
		if remaining < 0 {
			return fmt.Errorf("merge report exceeds %d retained events; narrow the query scope", MaxMergeReportEvents)
		}
		entry, valid, err := scanQueueAdmission(rows)
		if err != nil {
			return err
		}
		if !valid {
			report.UnverifiedQueueEvents++
			continue
		}
		key := queueEntryKey{entry.Provider, entry.RepositoryAPIURL, entry.EntryID}
		if previous, exists := entries[key]; exists {
			if previous.InstanceID != entry.InstanceID || previous.Gaggle != entry.Gaggle || previous.RunID != entry.RunID || previous.QueueAdmission != entry.QueueAdmission {
				conflicts[key] = true
			}
			continue
		}
		entries[key] = entry
	}
	if err := rows.Err(); err != nil {
		return err
	}
	report.QueueAdmissions = []AcceptedQueueEntry{}
	report.ConflictingQueueEntries = len(conflicts)
	for key, entry := range entries {
		if conflicts[key] || !matchesMergeQuery(ConfirmedMerge{InstanceID: entry.InstanceID, Gaggle: entry.Gaggle, RepositoryAPIURL: entry.RepositoryAPIURL}, query) {
			continue
		}
		report.QueueAdmissions = append(report.QueueAdmissions, entry)
	}
	sort.Slice(report.QueueAdmissions, func(i, j int) bool {
		a, b := report.QueueAdmissions[i], report.QueueAdmissions[j]
		if !a.OccurredAt.Equal(b.OccurredAt) {
			return a.OccurredAt.Before(b.OccurredAt)
		}
		return a.RepositoryAPIURL+"\x00"+a.EntryID < b.RepositoryAPIURL+"\x00"+b.EntryID
	})
	return nil
}

func scanQueueAdmission(rows *sql.Rows) (AcceptedQueueEntry, bool, error) {
	var entry AcceptedQueueEntry
	var pullID string
	var timestamp, raw sql.NullString
	if err := rows.Scan(&entry.Provider, &pullID, &entry.RunID, &timestamp, &entry.Gaggle, &entry.InstanceID, &raw); err != nil {
		return entry, false, err
	}
	var err error
	if entry.OccurredAt, err = parseTime(timestamp); err != nil {
		return entry, false, err
	}
	var fields struct {
		Admission *providers.QueueAdmission `json:"queueAdmission"`
	}
	if !raw.Valid || json.Unmarshal([]byte(raw.String), &fields) != nil || fields.Admission == nil || !instance.ValidIdentity(entry.InstanceID) || entry.Provider != "github" {
		return entry, false, nil
	}
	a := fields.Admission
	if a.PullID != pullID || !canonicalMergePullID(a.PullID) || !canonicalMergeRepository(a.RepositoryAPIURL) || len(a.EntryID) > 256 || strings.TrimSpace(a.EntryID) == "" || len(a.ExpectedHeadSHA) > 128 || a.EnqueuedAt.IsZero() {
		return entry, false, nil
	}
	entry.QueueAdmission = *a
	return entry, true, nil
}
