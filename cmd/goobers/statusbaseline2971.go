package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/baseline"
	"github.com/goobers/goobers/internal/instance"
)

// Shared-baseline visibility (#2971).
//
// When the target branch itself fails deterministic CI, every affected run is
// parked against ONE durable blocker instead of spending a repass re-deriving
// that there is nothing branch-local to fix. That is the right routing, but on
// its own it is invisible: an operator sees N quiet runs and no reason. Status
// therefore renders the blocker registry — which command is red on the base,
// what its signature is, and exactly which subjects are waiting on it — so the
// one repair that unblocks all of them is obvious.
//
// This is a read-only, local-state path: it opens the instance's baseline store
// and makes no provider call, so it costs nothing on an instance that never
// enabled base-health detection (the file simply does not exist).
const (
	// statusBaselineBlockerListLimit bounds how many blockers status prints;
	// the count line always reports the full total.
	statusBaselineBlockerListLimit = 5
	// statusBaselineWaiterListLimit bounds the subjects listed per blocker.
	statusBaselineWaiterListLimit = 10
)

// statusBaselineBlocker is one shared baseline failure and the subjects parked
// on it.
type statusBaselineBlocker struct {
	Key        string    `json:"key"`
	Repo       string    `json:"repo"`
	Command    string    `json:"command"`
	Signature  string    `json:"signature,omitempty"`
	BaseSHA    string    `json:"baseSha,omitempty"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	Waiting    []string  `json:"waiting"`
	// WaitingTotal counts every parked subject, including those beyond
	// statusBaselineWaiterListLimit.
	WaitingTotal int `json:"waitingTotal"`
}

// statusBaselineBlockers is the snapshot behind the status section.
type statusBaselineBlockers struct {
	Total    int                     `json:"total"`
	Waiting  int                     `json:"waiting"`
	Blockers []statusBaselineBlocker `json:"blockers"`
}

// loadStatusBaselineBlockers reads the instance's unresolved shared blockers.
// A missing store (detection never enabled, or never a red base) is an empty
// snapshot, not an error.
func loadStatusBaselineBlockers(l instance.Layout) (statusBaselineBlockers, error) {
	store, err := baseline.OpenStore(filepath.Join(l.Root, baselineStateFileName))
	if err != nil {
		return statusBaselineBlockers{}, err
	}
	var snapshot statusBaselineBlockers
	for _, blocker := range store.Blockers("") {
		if blocker.Resolved || len(blocker.Waiting) == 0 {
			continue
		}
		snapshot.Total++
		snapshot.Waiting += len(blocker.Waiting)
		if len(snapshot.Blockers) >= statusBaselineBlockerListLimit {
			continue
		}
		entry := statusBaselineBlocker{
			Key:          blocker.Key,
			Repo:         blocker.Repo,
			Command:      baseline.CommandDisplay(blocker.Command),
			Signature:    blocker.Signature,
			LastSeenAt:   blocker.LastSeenAt,
			WaitingTotal: len(blocker.Waiting),
		}
		if len(blocker.BaseSHAs) > 0 {
			entry.BaseSHA = blocker.BaseSHAs[len(blocker.BaseSHAs)-1]
		}
		for _, waiter := range blocker.Waiting {
			if len(entry.Waiting) >= statusBaselineWaiterListLimit {
				break
			}
			entry.Waiting = append(entry.Waiting, waiter.Subject)
		}
		snapshot.Blockers = append(snapshot.Blockers, entry)
	}
	return snapshot, nil
}

func baselineBlockerStatusText(snapshot statusBaselineBlockers, now time.Time) string {
	if snapshot.Total == 0 {
		return ""
	}
	var text strings.Builder
	fmt.Fprintf(&text,
		"Shared baseline failures (target branch already red — %d subject(s) parked): %d\n",
		snapshot.Waiting, snapshot.Total,
	)
	for _, blocker := range snapshot.Blockers {
		waiting := strings.Join(blocker.Waiting, ", ")
		if more := blocker.WaitingTotal - len(blocker.Waiting); more > 0 {
			waiting += fmt.Sprintf(", ... and %d more", more)
		}
		fmt.Fprintf(&text, "  %s %s — waiting: %s\n", blocker.Key, blocker.Command, waiting)
		detail := fmt.Sprintf("    base %s, last seen %s", shortBaselineSHA(blocker.BaseSHA), formatLastActivity(now, blocker.LastSeenAt))
		if blocker.Signature != "" {
			detail += "; " + blocker.Signature
		}
		text.WriteString(detail + "\n")
	}
	if more := snapshot.Total - len(snapshot.Blockers); more > 0 {
		fmt.Fprintf(&text, "  ... and %d more\n", more)
	}
	text.WriteString("  These runs are waiting on one repair to the target branch, not on their own diffs.\n")
	return text.String()
}

func baselineBlockerStatusUnavailableText(err error) string {
	return fmt.Sprintf("Shared baseline failures unavailable: %v\n", err)
}

func shortBaselineSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "unknown"
	}
	return sha
}
