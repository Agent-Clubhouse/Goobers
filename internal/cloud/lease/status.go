package lease

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// StatusReport is a DB-clock snapshot of lease authority and recent history.
type StatusReport struct {
	ObservedAt time.Time      `json:"observedAt"`
	Leases     []Current      `json:"leases"`
	History    []HistoryEntry `json:"history"`
	Truncated  bool           `json:"truncated,omitempty"`
}

// Current joins fenced authority to its renewable heartbeat.
type Current struct {
	InstanceID      string     `json:"instanceId"`
	Name            string     `json:"name"`
	Holder          string     `json:"holder"`
	Epoch           int64      `json:"epoch"`
	HeartbeatHolder string     `json:"heartbeatHolder,omitempty"`
	HeartbeatEpoch  int64      `json:"heartbeatEpoch,omitempty"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
	Expired         bool       `json:"expired"`
}

// HistoryEntry is one bounded lease ownership interval.
type HistoryEntry struct {
	InstanceID  string     `json:"instanceId"`
	Name        string     `json:"name"`
	Epoch       int64      `json:"epoch"`
	Holder      string     `json:"holder"`
	AcquiredAt  time.Time  `json:"acquiredAt"`
	ReleasedAt  *time.Time `json:"releasedAt,omitempty"`
	ReleaseKind string     `json:"releaseKind,omitempty"`
}

// Status reads all current leases and the 100 most recent ownership intervals.
func Status(ctx context.Context, pool *pgxpool.Pool) (StatusReport, error) {
	var report StatusReport
	if err := pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&report.ObservedAt); err != nil {
		return StatusReport{}, fmt.Errorf("read database clock: %w", err)
	}
	rows, err := pool.Query(ctx, `
		SELECT l.instance_id, l.name, l.holder, l.epoch,
		       COALESCE(h.holder, ''), COALESCE(h.epoch, 0), h.expires_at,
		       COALESCE(h.expires_at <= clock_timestamp(), true)
		FROM control.lease l
		LEFT JOIN control.lease_heartbeat h USING (instance_id, name)
		ORDER BY l.instance_id, l.name
		LIMIT 1001`)
	if err != nil {
		return StatusReport{}, fmt.Errorf("read cloud leases: %w", err)
	}
	for rows.Next() {
		var lease Current
		if err := rows.Scan(
			&lease.InstanceID, &lease.Name, &lease.Holder, &lease.Epoch,
			&lease.HeartbeatHolder, &lease.HeartbeatEpoch, &lease.ExpiresAt, &lease.Expired,
		); err != nil {
			rows.Close()
			return StatusReport{}, fmt.Errorf("decode cloud lease: %w", err)
		}
		report.Leases = append(report.Leases, lease)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		if len(report.Leases) > 1000 {
			report.Leases = report.Leases[:1000]
			report.Truncated = true
		}
		return StatusReport{}, fmt.Errorf("read cloud leases: %w", err)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `
		SELECT instance_id, name, epoch, holder, acquired_at, released_at,
		       COALESCE(release_kind, '')
		FROM control.lease_history
		ORDER BY acquired_at DESC, instance_id, name, epoch DESC
		LIMIT 100`)
	if err != nil {
		return StatusReport{}, fmt.Errorf("read cloud lease history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry HistoryEntry
		if err := rows.Scan(
			&entry.InstanceID, &entry.Name, &entry.Epoch, &entry.Holder,
			&entry.AcquiredAt, &entry.ReleasedAt, &entry.ReleaseKind,
		); err != nil {
			return StatusReport{}, fmt.Errorf("decode cloud lease history: %w", err)
		}
		report.History = append(report.History, entry)
	}
	if err := rows.Err(); err != nil {
		return StatusReport{}, fmt.Errorf("read cloud lease history: %w", err)
	}
	if report.Leases == nil {
		report.Leases = []Current{}
	}
	if report.History == nil {
		report.History = []HistoryEntry{}
	}
	return report, nil
}
