package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

func deliveryReferenceClosed(item providers.WorkItem, lookupPR func(string) (providers.PullRequestSummary, error)) (bool, error) {
	data, err := json.Marshal(item.Raw)
	if err != nil {
		return false, fmt.Errorf("read provider reference kind: %w", err)
	}
	var kind struct {
		PullRequest *json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &kind); err != nil {
		return false, fmt.Errorf("read provider reference kind: %w", err)
	}
	if kind.PullRequest == nil {
		return strings.EqualFold(item.State, "closed"), nil
	}
	pr, err := lookupPR(item.ID)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(pr.State, "closed") && !pr.Merged {
		return false, fmt.Errorf("PR #%s closed without merging; it is not delivery evidence", item.ID)
	}
	return pr.Merged, nil
}

func TestDeliveryReferenceRequiresPRMergeNotJustClosure(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		raw                           any
		state                         string
		merged, wantClosed, wantError bool
	}{
		{"closed issue", map[string]any{}, "closed", false, true, false},
		{"open issue", map[string]any{}, "open", false, false, false},
		{"merged PR", map[string]any{"pull_request": map[string]string{"url": "native"}}, "closed", true, true, false},
		{"abandoned PR", map[string]any{"pull_request": map[string]string{"url": "native"}}, "closed", false, false, true},
		{"open PR", map[string]any{"pull_request": map[string]string{"url": "native"}}, "open", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed, err := deliveryReferenceClosed(providers.WorkItem{ID: "42", State: tc.state, Raw: tc.raw}, func(id string) (providers.PullRequestSummary, error) {
				if id != "42" {
					t.Fatalf("wrong PR lookup: %s", id)
				}
				return providers.PullRequestSummary{State: tc.state, Merged: tc.merged}, nil
			})
			if closed != tc.wantClosed || (err != nil) != tc.wantError {
				t.Fatalf("closed=%v err=%v", closed, err)
			}
		})
	}
}

// Shared by the scheduled GitHub-aware test and deterministic fixtures. Pending
// work is separate from delivered evidence, so a fully closed partial delivery
// does not falsely imply that the complete design shipped.
func reconcileDelivery(doc document, resolve func(string) (closed, known bool)) []string {
	refs := uniqueSorted(append(append(append([]string{}, doc.DeliveredBy...), doc.Tracking...), doc.Remaining...))
	if len(refs) == 0 {
		return nil
	}
	var problems, open, closedPending []string
	for _, ref := range refs {
		closed, known := resolve(ref)
		if !known {
			problems = append(problems, fmt.Sprintf("%s: cannot resolve delivery reference %s", doc.Path, ref))
			continue
		}
		if !closed {
			open = append(open, ref)
		}
		if closed && contains(doc.Remaining, ref) {
			closedPending = append(closedPending, ref)
		}
	}
	if len(problems) != 0 {
		return problems
	}
	if len(closedPending) > 0 {
		problems = append(problems, fmt.Sprintf("%s: Pending-delivery still names closed issue(s) %s; update the delivery ledger and scope delta", doc.Path, strings.Join(closedPending, ", ")))
	}
	switch doc.Status {
	case "implemented":
		if len(open) > 0 {
			problems = append(problems, fmt.Sprintf("%s is marked `implemented` but its delivery/tracking ledger has open issue(s) %s", doc.Path, strings.Join(open, ", ")))
		}
	case "draft", "approved":
		if len(open) == 0 {
			problems = append(problems, fmt.Sprintf("%s is marked `%s` but every issue in its complete delivery/tracking ledger is closed; verify completion or record Pending-delivery and Scope-delta", doc.Path, doc.Status))
		}
	}
	return problems
}

func TestReconcileDeliveryDistinguishesPartialFromComplete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		doc    document
		states map[string]bool
		want   string
	}{
		{"partial", document{Status: "approved", DeliveredBy: []string{"#1"}, Remaining: []string{"#2"}}, map[string]bool{"#1": true, "#2": false}, ""},
		{"all closed", document{Status: "approved", DeliveredBy: []string{"#1"}}, map[string]bool{"#1": true}, "every issue"},
		{"tracking closed without deliveries", document{Status: "draft", Tracking: []string{"#1"}}, map[string]bool{"#1": true}, "every issue"},
		{"tracking open", document{Status: "approved", Tracking: []string{"#2"}, DeliveredBy: []string{"#1"}}, map[string]bool{"#1": true, "#2": false}, ""},
		{"stale pending", document{Status: "approved", Remaining: []string{"#1", "#2"}}, map[string]bool{"#1": true, "#2": false}, "Pending-delivery still names closed"},
		{"overstated", document{Status: "implemented", DeliveredBy: []string{"#1"}}, map[string]bool{"#1": false}, "open issue"},
		{"unknown", document{Status: "approved", DeliveredBy: []string{"#1"}}, map[string]bool{}, "cannot resolve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := reconcileDelivery(tc.doc, func(ref string) (bool, bool) { state, known := tc.states[ref]; return state, known })
			if tc.want == "" && len(problems) != 0 {
				t.Fatalf("unexpected: %v", problems)
			}
			if tc.want != "" && !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Fatalf("problems=%v want=%s", problems, tc.want)
			}
		})
	}
}
