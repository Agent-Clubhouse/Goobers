package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	adoHeadsRefPrefix = "refs/heads/"
	// adoRefsPageSize bounds one page of the Git Refs list; the ref sweep
	// follows x-ms-continuationtoken for the rest.
	adoRefsPageSize = 1000
)

// adoRef is one entry of the Git Refs API's list and update responses.
// Success and UpdateStatus are populated only by ref updates (POST refs);
// a list response leaves them nil/empty.
type adoRef struct {
	Name         string `json:"name"`
	ObjectID     string `json:"objectId"`
	URL          string `json:"url"`
	Success      *bool  `json:"success,omitempty"`
	UpdateStatus string `json:"updateStatus,omitempty"`
}

// adoPushList is the minimal shape of the Git Pushes list response.
type adoPushList struct {
	Value []struct {
		PushID     int       `json:"pushId"`
		Date       time.Time `json:"date"`
		RefUpdates []struct {
			Name string `json:"name"`
		} `json:"refUpdates"`
	} `json:"value"`
}

// Compile-time proof that Azure DevOps satisfies the reconciliation surface
// `goobers run continue` and `goobers reconcile-branches` require (#5900).
var _ BranchReconciliationProvider = (*ADOProvider)(nil)

// ListBranches returns a bounded lexicographic page of Azure DevOps branches
// whose names start with req.Prefix and sort after req.After. The Git Refs
// API's filter is a server-side starts-with match on "heads/<prefix>"; every
// page is read before sorting so the result does not depend on the server's
// ordering, and After lets repeated bounded sweeps progress without page
// numbers that shift when an earlier branch is deleted.
func (p *ADOProvider) ListBranches(ctx context.Context, req ListBranchesRequest) ([]BranchSummary, error) {
	if err := requireRepo(req.Repository); err != nil {
		return nil, err
	}
	if req.Prefix == "" {
		return nil, fmt.Errorf("branch prefix is required")
	}
	if req.Limit < 1 {
		return nil, fmt.Errorf("branch limit must be positive")
	}
	refs, err := p.listHeadRefs(ctx, req.Repository, req.Prefix)
	if err != nil {
		return nil, err
	}
	branches := make([]BranchSummary, 0, req.Limit)
	for _, ref := range refs {
		name, ok := strings.CutPrefix(ref.Name, adoHeadsRefPrefix)
		if !ok || !strings.HasPrefix(name, req.Prefix) || (req.After != "" && name <= req.After) {
			continue
		}
		branches = append(branches, BranchSummary{Name: name, SHA: ref.ObjectID, URL: ref.URL})
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].Name < branches[j].Name })
	if len(branches) > req.Limit {
		branches = branches[:req.Limit]
	}
	return branches, nil
}

// GetBranch reads one exact Azure DevOps branch ref and the time of its most
// recent push for reconciliation's pre-delete staleness check. A missing ref
// is reported as found=false, separately from provider failure. A branch with
// no recorded push leaves LastActivityAt nil, which reconciliation treats as
// unknown activity and never as stale.
func (p *ADOProvider) GetBranch(ctx context.Context, repo RepositoryRef, name string) (BranchSummary, bool, error) {
	if err := requireRepo(repo); err != nil {
		return BranchSummary{}, false, err
	}
	name = strings.TrimPrefix(name, adoHeadsRefPrefix)
	if name == "" {
		return BranchSummary{}, false, fmt.Errorf("branch name is required")
	}
	ref, found, err := p.findHeadRef(ctx, repo, name)
	if err != nil || !found {
		return BranchSummary{}, false, err
	}
	branch := BranchSummary{Name: name, SHA: ref.ObjectID, URL: ref.URL}
	activity, err := p.lastPushTime(ctx, repo, name)
	if err != nil {
		return BranchSummary{}, false, err
	}
	branch.LastActivityAt = activity
	return branch, true, nil
}

// findHeadRef resolves exactly refs/heads/<name>. The Git Refs filter is a
// starts-with match, so "run-1" also returns "run-10" and "run-1/x"; only an
// exact name match counts.
func (p *ADOProvider) findHeadRef(ctx context.Context, repo RepositoryRef, name string) (adoRef, bool, error) {
	refs, err := p.listHeadRefs(ctx, repo, name)
	if err != nil {
		return adoRef{}, false, err
	}
	want := adoHeadsRefPrefix + name
	for _, ref := range refs {
		if ref.Name == want && ref.ObjectID != "" {
			return ref, true, nil
		}
	}
	return adoRef{}, false, nil
}

// listHeadRefs reads every branch ref starting with prefix, following
// x-ms-continuationtoken across pages.
func (p *ADOProvider) listHeadRefs(ctx context.Context, repo RepositoryRef, prefix string) ([]adoRef, error) {
	base, err := p.repoURL(repo, "refs")
	if err != nil {
		return nil, err
	}
	var refs []adoRef
	seen := map[string]bool{}
	continuation := ""
	for {
		values := url.Values{
			"filter": []string{"heads/" + prefix},
			"$top":   []string{strconv.Itoa(adoRefsPageSize)},
		}
		if continuation != "" {
			values.Set("continuationToken", continuation)
		}
		endpoint, err := addQuery(base, values)
		if err != nil {
			return nil, err
		}
		resp, err := p.send(ctx, http.MethodGet, endpoint, nil, "")
		if err != nil {
			return nil, err
		}
		var page struct {
			Value []adoRef `json:"value"`
		}
		if err := readJSONResponse(resp, http.MethodGet, endpoint, &page); err != nil {
			return nil, err
		}
		refs = append(refs, page.Value...)
		next := strings.TrimSpace(resp.Header.Get("x-ms-continuationtoken"))
		if next == "" {
			return refs, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("ado refs: continuation token repeated")
		}
		seen[next] = true
		continuation = next
	}
}

// lastPushTime returns the date of the newest push that updated
// refs/heads/<name>, or nil when ADO records none. The Pushes API lists
// newest first; the returned push must name the ref so a server that ignored
// the refName filter cannot pass another branch's activity off as this one's.
func (p *ADOProvider) lastPushTime(ctx context.Context, repo RepositoryRef, name string) (*time.Time, error) {
	endpoint, err := p.repoURL(repo, "pushes")
	if err != nil {
		return nil, err
	}
	refName := adoHeadsRefPrefix + name
	endpoint, err = addQuery(endpoint, url.Values{
		"searchCriteria.refName":           []string{refName},
		"searchCriteria.includeRefUpdates": []string{"true"},
		"$top":                             []string{"1"},
	})
	if err != nil {
		return nil, err
	}
	var out adoPushList
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	if len(out.Value) == 0 {
		return nil, nil
	}
	push := out.Value[0]
	for _, update := range push.RefUpdates {
		if update.Name == refName {
			if push.Date.IsZero() {
				return nil, nil
			}
			at := push.Date
			return &at, nil
		}
	}
	return nil, fmt.Errorf("ado push %d does not update %q", push.PushID, refName)
}
