package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MergeInventory reads actual landing times and mergers without checking CI,
// reviews, or authorship. It fails instead of returning partial totals when
// a scan bound is reached. GitHub pagination is not a transactional snapshot;
// a repeated PR (for example, from concurrent updates moving page boundaries)
// makes the scan fail rather than silently deduplicating an unstable inventory.
func (p *GitHubProvider) MergeInventory(ctx context.Context, req MergeInventoryRequest) ([]MergeInventoryEntry, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return nil, err
	}
	if req.Since.IsZero() || !req.Until.After(req.Since) || req.Until.Sub(req.Since) > 90*24*time.Hour || req.Limit < 1 || req.Limit > 10000 {
		return nil, fmt.Errorf("merge inventory requires an increasing window of at most 90 days and a limit of 1–10000")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	repository, err := joinURL(p.BaseURL, "repos", strings.ToLower(req.Repository.Owner), strings.ToLower(req.Repository.Name))
	if err != nil {
		return nil, err
	}
	endpoint, err := addQuery(repository+"/pulls", url.Values{"state": {"closed"}, "sort": {"updated"}, "direction": {"desc"}})
	if err != nil {
		return nil, err
	}
	entries := []MergeInventoryEntry{}
	seen := map[int]bool{}
	pages, scanned := 0, 0
	err = p.getAllPagesWithContext(ctx, endpoint, func(raw []byte, page pageContext) error {
		pages++
		if pages > 101 {
			return fmt.Errorf("merge inventory exceeds page bound; narrow the window")
		}
		records, err := decodeMergeInventoryPage(raw)
		if err != nil {
			return err
		}
		for _, record := range records {
			scanned++
			if scanned > req.Limit {
				return fmt.Errorf("merge inventory exceeds %d raw records; narrow the window", req.Limit)
			}
			if record.Number <= 0 || record.UpdatedAt.IsZero() || seen[record.Number] {
				return fmt.Errorf("invalid or repeated PR in merge inventory")
			}
			seen[record.Number] = true
			if record.UpdatedAt.Before(req.Since) {
				return errStopPaging
			}
			if record.MergedAt == nil || record.MergedAt.Before(req.Since) || !record.MergedAt.Before(req.Until) {
				continue
			}
			entry, err := p.mergeInventoryDetail(ctx, repository, record)
			if err != nil {
				return err
			}
			entries = append(entries, entry)
		}
		if scanned == req.Limit && page.HasNext {
			return fmt.Errorf("merge inventory reached %d raw records with pages remaining; narrow the window", req.Limit)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func decodeMergeInventoryPage(raw []byte) ([]githubPullRequestDetail, error) {
	var records []githubPullRequestDetail
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("decode merge inventory page: %w", err)
	}
	if records == nil {
		return nil, fmt.Errorf("merge inventory page must be an array, not null")
	}
	return records, nil
}

func (p *GitHubProvider) mergeInventoryDetail(ctx context.Context, repository string, listed githubPullRequestDetail) (MergeInventoryEntry, error) {
	id := strconv.Itoa(listed.Number)
	var detail struct {
		githubPullRequestDetail
		MergedBy githubUser `json:"merged_by"`
	}
	if err := p.do(ctx, http.MethodGet, repository+"/pulls/"+id, nil, &detail); err != nil {
		return MergeInventoryEntry{}, err
	}
	if detail.Number != listed.Number || !detail.Merged || detail.MergedAt == nil || !detail.MergedAt.Equal(*listed.MergedAt) {
		return MergeInventoryEntry{}, fmt.Errorf("PR %s changed or lacks merge confirmation during inventory", id)
	}
	confirmation := newMergeConfirmation(repository, id, detail.MergeCommitSHA)
	if confirmation == nil {
		return MergeInventoryEntry{}, fmt.Errorf("invalid merge inventory repository address")
	}
	return MergeInventoryEntry{Provider: ProviderGitHub, RepositoryAPIURL: confirmation.RepositoryAPIURL, PullID: id, MergeSHA: detail.MergeCommitSHA, MergedAt: detail.MergedAt.UTC(), MergedBy: detail.MergedBy.Login}, nil
}
