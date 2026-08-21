package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// labelEventHistory serves a synthetic repository issue-event history, newest
// first (GitHub's ordering), one event per page so page counts read directly as
// "how much history did this walk pay for".
type labelEventHistory struct {
	t *testing.T
	// count is how many ready-label events exist; ids run 1..count with the
	// newest id served first.
	count int
	// pages records the page numbers actually requested.
	pages []int
	// oldestFirst inverts the ordering to model a server that does NOT sort
	// descending — the walk must stay correct, just not cheap.
	oldestFirst bool
	// limit/remaining/drain model an absolute rate-limit window that drains as
	// the walk proceeds. limit == 0 omits the headers entirely.
	limit, remaining, drain int
	server                  *httptest.Server
}

func newLabelEventHistory(t *testing.T, count int) *labelEventHistory {
	t.Helper()
	h := &labelEventHistory{t: t, count: count}
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.server.Close)
	return h
}

func (h *labelEventHistory) serve(w http.ResponseWriter, r *http.Request) {
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			h.t.Fatalf("page = %q", raw)
		}
		page = parsed
	}
	h.pages = append(h.pages, page)
	if h.limit > 0 {
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(h.limit))
		remaining := h.remaining
		if remaining < 0 {
			remaining = 0
		}
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		h.remaining -= h.drain
	}
	if page > h.count {
		writeJSON(h.t, w, []map[string]any{})
		return
	}
	if page < h.count {
		w.Header().Set("Link", fmt.Sprintf("<%s%s?page=%d&per_page=100>; rel=%q",
			h.server.URL, r.URL.Path, page+1, "next"))
	}
	// Two events per page, ordered within the page the same way the history is
	// ordered overall, so the walk can detect the server's ordering.
	seq := h.count - page + 1
	if h.oldestFirst {
		seq = page
	}
	ready := labelEvent(int64(seq)*2, seq, LabelReady)
	other := labelEvent(int64(seq)*2-1, seq, "other")
	if h.oldestFirst {
		writeJSON(h.t, w, []map[string]any{other, ready})
		return
	}
	writeJSON(h.t, w, []map[string]any{ready, other})
}

// labelEvent builds one repository issue event. Ready-label events carry even
// ids and unrelated-label events odd ids, so a cursor test can prove the walk
// advances past events it does not collect.
func labelEvent(id int64, seq int, label string) map[string]any {
	return map[string]any{
		"id":         id,
		"event":      "labeled",
		"created_at": time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Hour),
		"label":      map[string]string{"name": label},
		"issue":      map[string]any{"number": seq},
	}
}

func (h *labelEventHistory) provider() *GitHubProvider {
	return NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = h.server.URL })
}

func (h *labelEventHistory) scan(t *testing.T, scan LabelTransitionScan) LabelTransitionScanResult {
	t.Helper()
	h.pages = nil
	result, err := h.provider().ScanWorkItemLabelTransitions(
		context.Background(), RepositoryRef{Owner: "acme", Name: "app"}, LabelReady, scan)
	if err != nil {
		t.Fatalf("ScanWorkItemLabelTransitions: %v", err)
	}
	return result
}

// TestScanWorkItemLabelTransitionsStopsAtCursor is #3392's provider-level
// regression: a resumed walk must cost the pages holding new events, not the
// whole history.
func TestScanWorkItemLabelTransitionsStopsAtCursor(t *testing.T) {
	history := newLabelEventHistory(t, 20)

	full := history.scan(t, LabelTransitionScan{})
	if len(full.Transitions) != 20 {
		t.Fatalf("full scan transitions = %d, want 20", len(full.Transitions))
	}
	if full.Pages != 20 || full.Truncated || full.ReachedCursor {
		t.Fatalf("full scan = %#v", full)
	}
	if full.HighEventID != 40 {
		t.Fatalf("full scan HighEventID = %d, want 40", full.HighEventID)
	}

	// Resume from two pages back: the walk must read page 1 and page 2 and stop
	// there, not walk all 20 pages again.
	resumed := history.scan(t, LabelTransitionScan{AfterEventID: 36})
	if resumed.Pages != 3 {
		t.Fatalf("resumed pages = %d (%v), want 3", resumed.Pages, history.pages)
	}
	if !resumed.ReachedCursor || resumed.Truncated {
		t.Fatalf("resumed = %#v", resumed)
	}
	if len(resumed.Transitions) != 2 ||
		resumed.Transitions[0].EventID != 38 || resumed.Transitions[1].EventID != 40 {
		t.Fatalf("resumed transitions = %#v", resumed.Transitions)
	}
	if resumed.HighEventID != 40 {
		t.Fatalf("resumed HighEventID = %d, want 40", resumed.HighEventID)
	}
}

// TestScanWorkItemLabelTransitionsFullWalkWhenServerIsOldestFirst pins the
// safety property behind the cursor: newest-first ordering is detected, not
// assumed. Against an oldest-first server the walk reads everything — today's
// cost — and still returns exactly the transitions above the cursor.
func TestScanWorkItemLabelTransitionsFullWalkWhenServerIsOldestFirst(t *testing.T) {
	history := newLabelEventHistory(t, 6)
	history.oldestFirst = true

	// Ready-label events carry ids 2,4,6,8,10,12; four of them sit above 4.
	resumed := history.scan(t, LabelTransitionScan{AfterEventID: 4})
	if resumed.Pages != 6 {
		t.Fatalf("pages = %d (%v), want the whole history", resumed.Pages, history.pages)
	}
	if len(resumed.Transitions) != 4 {
		t.Fatalf("transitions = %#v, want every event above the cursor", resumed.Transitions)
	}
	for _, transition := range resumed.Transitions {
		if transition.EventID <= 4 {
			t.Fatalf("transition %#v is at or below the cursor", transition)
		}
	}
}

// TestScanWorkItemLabelTransitionsStopsAtQuotaFloor is #3392's other half: the
// walk must stand down while the shared credential still has budget, and say so,
// rather than paging on to a 403 at remaining 0.
func TestScanWorkItemLabelTransitionsStopsAtQuotaFloor(t *testing.T) {
	history := newLabelEventHistory(t, 50)
	history.limit, history.remaining, history.drain = 100, 20, 1

	result := history.scan(t, LabelTransitionScan{MinQuotaFraction: 0.15})
	if !result.Truncated || result.StopReason != LabelTransitionScanStopQuotaFloor {
		t.Fatalf("result = %#v, want a quota-floor stop", result)
	}
	// The window starts at 20/100 and drains one per page, so page 7 is the
	// first to report a remaining (14) strictly below the floor of 15.
	if result.Pages != 7 {
		t.Fatalf("pages = %d, want the walk to stop as it crosses the floor", result.Pages)
	}
	if !result.QuotaKnown || result.QuotaRemaining != 14 || result.QuotaLimit != 100 {
		t.Fatalf("quota = %d/%d known=%v", result.QuotaRemaining, result.QuotaLimit, result.QuotaKnown)
	}
}

// TestScanWorkItemLabelTransitionsQuotaFloorAllowsCompletion guards the
// opposite direction: a healthy window must not make a completable walk look
// truncated.
func TestScanWorkItemLabelTransitionsQuotaFloorAllowsCompletion(t *testing.T) {
	history := newLabelEventHistory(t, 4)
	history.limit, history.remaining, history.drain = 5000, 5000, 1

	result := history.scan(t, LabelTransitionScan{MinQuotaFraction: 0.10})
	if result.Truncated || result.StopReason != "" {
		t.Fatalf("result = %#v, want a complete walk", result)
	}
	if len(result.Transitions) != 4 {
		t.Fatalf("transitions = %#v, want the whole history", result.Transitions)
	}
}

// TestScanWorkItemLabelTransitionsPageBudget bounds a first-run full scan, and
// proves the bound never fires on a walk that had already reached the end.
func TestScanWorkItemLabelTransitionsPageBudget(t *testing.T) {
	history := newLabelEventHistory(t, 10)

	bounded := history.scan(t, LabelTransitionScan{MaxPages: 3})
	if !bounded.Truncated || bounded.StopReason != LabelTransitionScanStopPageBudget || bounded.Pages != 3 {
		t.Fatalf("bounded = %#v", bounded)
	}

	exact := history.scan(t, LabelTransitionScan{MaxPages: 10})
	if exact.Truncated || exact.Pages != 10 {
		t.Fatalf("exact = %#v, want a complete walk that never reports truncation", exact)
	}
}
