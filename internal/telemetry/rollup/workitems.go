package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
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
	Repository    string
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

type RelatedWorkItem struct {
	Provider   string
	Repository string
	Kind       string
	ExternalID string
	URL        string
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
		WITH normalized AS (
			SELECT
				pm.*,
				COALESCE(r.gaggle, '') AS source_gaggle,
				CASE
					WHEN instr(COALESCE(pm.url, ''), '#') > 0
						THEN substr(pm.url, 1, instr(pm.url, '#') - 1)
					ELSE COALESCE(pm.url, '')
				END AS canonical_url
			FROM provider_mutations pm
			LEFT JOIN runs r ON r.run_id = pm.run_id
			WHERE pm.kind IN ('pr', 'issue')
				AND (? = '' OR pm.provider = ?)
				AND (? = '' OR pm.kind = ?)
		),
		resolved AS (
			SELECT
				normalized.*,
				COALESCE(
					NULLIF(canonical_url, ''),
					MAX(NULLIF(canonical_url, '')) OVER (
						PARTITION BY provider, kind, external_id, source_gaggle
					),
					''
				) AS item_url
			FROM normalized
		),
		ranked AS (
			SELECT
				pm.provider,
				pm.kind,
				pm.external_id,
				pm.item_url,
				COUNT(*) OVER (
					PARTITION BY pm.provider, pm.kind, pm.external_id, pm.item_url
				) AS action_count,
				pm.operation,
				pm.occurred_at,
				pm.run_id,
				COALESCE(r.gaggle, '') AS gaggle,
				COALESCE(r.workflow, '') AS workflow,
				COALESCE(r.status, '') AS status,
				ROW_NUMBER() OVER (
					PARTITION BY pm.provider, pm.kind, pm.external_id, pm.item_url
					ORDER BY julianday(pm.occurred_at) DESC, pm.occurred_at DESC, pm.run_id DESC, pm.seq DESC
				) AS item_rank
			FROM resolved pm
			LEFT JOIN runs r ON r.run_id = pm.run_id
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
		item.Repository = workItemRepository(item.Provider, item.URL)
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
	repository string,
	kind string,
	externalID string,
) ([]WorkItemAction, bool, error) {
	itemURL := workItemURL(provider, repository, kind, externalID)
	where := `pm.provider = ? AND pm.kind = ? AND pm.external_id = ?`
	args := []any{provider, kind, externalID}
	if itemURL != "" {
		where += ` AND (
			lower(pm.item_url) = lower(?)
			OR (
				pm.canonical_url = ''
				AND pm.gaggle IN (
					SELECT matching.gaggle
					FROM resolved matching
					WHERE lower(matching.item_url) = lower(?)
				)
			)
		)`
		args = append(args, itemURL, itemURL)
	}
	args = append(args, MaxWorkItemActions+1)
	rows, err := db.readDB().QueryContext(ctx, `
		WITH normalized AS (
			SELECT
				pm.*,
				COALESCE(r.gaggle, '') AS gaggle,
				COALESCE(r.workflow, '') AS workflow,
				COALESCE(r.status, '') AS status,
				CASE
					WHEN instr(COALESCE(pm.url, ''), '#') > 0
						THEN substr(pm.url, 1, instr(pm.url, '#') - 1)
					ELSE COALESCE(pm.url, '')
				END AS canonical_url
			FROM provider_mutations pm
			LEFT JOIN runs r ON r.run_id = pm.run_id
		),
		resolved AS (
			SELECT
				normalized.*,
				COALESCE(
					NULLIF(canonical_url, ''),
					MAX(NULLIF(canonical_url, '')) OVER (
						PARTITION BY provider, kind, external_id, gaggle
					),
					''
				) AS item_url
			FROM normalized
		)
		SELECT pm.run_id, pm.seq, pm.item_url, COALESCE(pm.operation, ''),
		       pm.occurred_at, pm.gaggle, pm.workflow, pm.status
		FROM resolved pm
		WHERE `+where+`
		ORDER BY julianday(pm.occurred_at) DESC, pm.occurred_at DESC, pm.run_id DESC, pm.seq DESC
		LIMIT ?`,
		args...)
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

func (db *DB) RelatedPullRequests(
	ctx context.Context,
	provider string,
	repository string,
	issueID string,
) ([]RelatedWorkItem, error) {
	issueURL := workItemURL(provider, repository, "issue", issueID)
	pullPrefix := workItemURL(provider, repository, "pr", "")
	if issueURL == "" || pullPrefix == "" {
		return []RelatedWorkItem{}, nil
	}
	rows, err := db.readDB().QueryContext(ctx, `
		WITH issue_gaggles AS (
			SELECT DISTINCT r.gaggle
			FROM provider_mutations pm
			JOIN runs r ON r.run_id = pm.run_id
			WHERE pm.provider = ? AND pm.kind = 'issue' AND pm.external_id = ?
				AND lower(CASE
					WHEN instr(COALESCE(pm.url, ''), '#') > 0
						THEN substr(pm.url, 1, instr(pm.url, '#') - 1)
					ELSE COALESCE(pm.url, '')
				END) = lower(?)
		),
		issue_runs AS (
			SELECT DISTINCT a.run_id
			FROM run_cost_attribution a
			JOIN runs r ON r.run_id = a.run_id
			WHERE a.provider = ? AND a.external_kind = 'issue' AND a.external_id = ?
				AND r.gaggle IN (SELECT gaggle FROM issue_gaggles)
		),
		related_ids AS (
			SELECT DISTINCT a.external_id
			FROM run_cost_attribution a
			WHERE a.provider = ? AND a.external_kind = 'pr'
				AND a.run_id IN (SELECT run_id FROM issue_runs)
		),
		normalized AS (
			SELECT
				pm.provider,
				pm.external_id,
				CASE
					WHEN instr(COALESCE(pm.url, ''), '#') > 0
						THEN substr(pm.url, 1, instr(pm.url, '#') - 1)
					ELSE COALESCE(pm.url, '')
				END AS item_url,
				pm.occurred_at,
				pm.seq
			FROM provider_mutations pm
			WHERE pm.provider = ? AND pm.kind = 'pr'
				AND pm.external_id IN (SELECT external_id FROM related_ids)
		),
		ranked AS (
			SELECT *,
				ROW_NUMBER() OVER (
					PARTITION BY provider, external_id, item_url
					ORDER BY julianday(occurred_at) DESC, occurred_at DESC, seq DESC
				) AS item_rank
			FROM normalized
			WHERE lower(item_url) LIKE lower(?) || '%'
		)
		SELECT provider, external_id, item_url
		FROM ranked
		WHERE item_rank = 1
		ORDER BY CAST(external_id AS INTEGER), external_id`,
		provider, issueID, issueURL,
		provider, issueID,
		provider,
		provider, pullPrefix)
	if err != nil {
		return nil, fmt.Errorf("rollup: query related pull requests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := []RelatedWorkItem{}
	for rows.Next() {
		var item RelatedWorkItem
		item.Kind = "pr"
		if err := rows.Scan(&item.Provider, &item.ExternalID, &item.URL); err != nil {
			return nil, fmt.Errorf("rollup: scan related pull request: %w", err)
		}
		item.Repository = workItemRepository(item.Provider, item.URL)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rollup: iterate related pull requests: %w", err)
	}
	return items, nil
}

func workItemURL(provider, repository, kind, externalID string) string {
	if !strings.EqualFold(provider, "github") || strings.TrimSpace(repository) == "" {
		return ""
	}
	segment := "issues"
	if kind == "pr" {
		segment = "pull"
	}
	base := "https://github.com/" + strings.Trim(repository, "/") + "/" + segment + "/"
	return base + externalID
}

func workItemRepository(provider, rawURL string) string {
	if !strings.EqualFold(provider, "github") {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 4 || (parts[2] != "issues" && parts[2] != "pull") {
		return ""
	}
	return parts[0] + "/" + parts[1]
}
