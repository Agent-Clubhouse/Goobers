package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const MaxWorkItemActions = 200

type WorkItemQuery struct {
	Provider string
	Kind     string
	Limit    int
}

type WorkItem struct {
	Provider      string
	Kind          string
	ExternalID    string
	URL           string
	ActionCount   int
	LastOperation string
	LastActionAt  time.Time
	LastRunID     string
	Gaggle        string
	Workflow      string
	RunStatus     string
}

type WorkItemAction struct {
	RunID      string
	Seq        uint64
	URL        string
	Operation  string
	OccurredAt time.Time
	Gaggle     string
	Workflow   string
	RunStatus  string
}

func (db *DB) WorkItems(ctx context.Context, query WorkItemQuery) ([]WorkItem, bool, error) {
	limit := query.Limit
	if limit <= 0 || limit > MaxWorkItemActions {
		limit = MaxWorkItemActions
	}
	rows, err := db.readDB().QueryContext(ctx, `
		WITH ranked AS (
			SELECT
				pm.provider,
				pm.kind,
				pm.external_id,
				COALESCE(MAX(pm.url) OVER (
					PARTITION BY pm.provider, pm.kind, pm.external_id
				), '') AS item_url,
				COUNT(*) OVER (
					PARTITION BY pm.provider, pm.kind, pm.external_id
				) AS action_count,
				pm.operation,
				pm.occurred_at,
				pm.run_id,
				COALESCE(r.gaggle, ''),
				COALESCE(r.workflow, ''),
				COALESCE(r.status, ''),
				ROW_NUMBER() OVER (
					PARTITION BY pm.provider, pm.kind, pm.external_id
					ORDER BY julianday(pm.occurred_at) DESC, pm.occurred_at DESC, pm.run_id DESC, pm.seq DESC
				) AS item_rank
			FROM provider_mutations pm
			LEFT JOIN runs r ON r.run_id = pm.run_id
			WHERE (? = '' OR pm.provider = ?)
				AND (? = '' OR pm.kind = ?)
		)
		SELECT provider, kind, external_id, item_url, action_count, operation,
		       occurred_at, run_id, gaggle, workflow, status
		FROM ranked
		WHERE item_rank = 1
		ORDER BY julianday(occurred_at) DESC, occurred_at DESC, provider, kind, external_id
		LIMIT ?`,
		query.Provider, query.Provider, query.Kind, query.Kind, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("rollup: query work items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]WorkItem, 0, limit)
	for rows.Next() {
		var item WorkItem
		var occurredAt sql.NullString
		if err := rows.Scan(
			&item.Provider,
			&item.Kind,
			&item.ExternalID,
			&item.URL,
			&item.ActionCount,
			&item.LastOperation,
			&occurredAt,
			&item.LastRunID,
			&item.Gaggle,
			&item.Workflow,
			&item.RunStatus,
		); err != nil {
			return nil, false, fmt.Errorf("rollup: scan work item: %w", err)
		}
		var err error
		if item.LastActionAt, err = parseTime(occurredAt); err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("rollup: iterate work items: %w", err)
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	return items, hasMore, nil
}

func (db *DB) WorkItemActions(
	ctx context.Context,
	provider string,
	kind string,
	externalID string,
) ([]WorkItemAction, bool, error) {
	rows, err := db.readDB().QueryContext(ctx, `
		SELECT pm.run_id, pm.seq, COALESCE(pm.url, ''), COALESCE(pm.operation, ''),
		       pm.occurred_at, COALESCE(r.gaggle, ''), COALESCE(r.workflow, ''),
		       COALESCE(r.status, '')
		FROM provider_mutations pm
		LEFT JOIN runs r ON r.run_id = pm.run_id
		WHERE pm.provider = ? AND pm.kind = ? AND pm.external_id = ?
		ORDER BY julianday(pm.occurred_at) DESC, pm.occurred_at DESC, pm.run_id DESC, pm.seq DESC
		LIMIT ?`,
		provider, kind, externalID, MaxWorkItemActions+1)
	if err != nil {
		return nil, false, fmt.Errorf("rollup: query work item actions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	actions := make([]WorkItemAction, 0, MaxWorkItemActions)
	for rows.Next() {
		var action WorkItemAction
		var occurredAt sql.NullString
		if err := rows.Scan(
			&action.RunID,
			&action.Seq,
			&action.URL,
			&action.Operation,
			&occurredAt,
			&action.Gaggle,
			&action.Workflow,
			&action.RunStatus,
		); err != nil {
			return nil, false, fmt.Errorf("rollup: scan work item action: %w", err)
		}
		var err error
		if action.OccurredAt, err = parseTime(occurredAt); err != nil {
			return nil, false, err
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("rollup: iterate work item actions: %w", err)
	}
	hasMore := len(actions) > MaxWorkItemActions
	if hasMore {
		actions = actions[:MaxWorkItemActions]
	}
	return actions, hasMore, nil
}
