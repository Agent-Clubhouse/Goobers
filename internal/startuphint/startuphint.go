// Package startuphint carries a starting daemon's own estimate of how long it
// may still take to become ready, on the HTTP 503 it answers while starting.
//
// A client that waits for readiness (the resident worker's blob-plane probe)
// derives its bound from these hints instead of guessing one fixed timeout:
// the daemon's startup budget grows with the work it found (#6895), and the
// progress token changes whenever startup visibly advances, which separates a
// slow daemon from a stuck one.
package startuphint

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// HeaderBudgetRemaining is the whole number of seconds left in the
	// daemon's derived startup budget. Zero means the budget is spent.
	HeaderBudgetRemaining = "Goobers-Startup-Budget-Remaining"
	// HeaderProgress is an opaque token that changes whenever startup
	// advances (a phase begins, a budget input is measured, or a phase reports
	// progress). Only equality is meaningful.
	HeaderProgress = "Goobers-Startup-Progress"
	// HeaderDaemon is an opaque token naming the daemon process that sent
	// the hints. A restarted daemon sends a new one, so its progress token
	// and budget are not mistaken for the previous process advancing.
	HeaderDaemon = "Goobers-Startup-Daemon"

	// maxBudgetRemaining bounds a parsed budget so a malformed or hostile
	// answer cannot stretch a client's wait without limit.
	maxBudgetRemaining = 24 * time.Hour
	maxTokenLen        = 64
)

// Hints is what a starting daemon advertised. The zero value advertises
// nothing.
type Hints struct {
	// BudgetRemaining is the time left in the daemon's startup budget; it is
	// meaningful only when HasBudget is true.
	BudgetRemaining time.Duration
	// HasBudget reports whether the daemon advertised a budget.
	HasBudget bool
	// Progress is the daemon's progress token, or "" when none was sent.
	Progress string
	// Daemon identifies the daemon process, or "" when none was sent.
	Daemon string
}

// Set writes hints onto h. Absent fields are left unset.
func Set(h http.Header, hints Hints) {
	if hints.HasBudget {
		remaining := hints.BudgetRemaining
		if remaining < 0 {
			remaining = 0
		}
		h.Set(HeaderBudgetRemaining, strconv.FormatInt(int64(remaining/time.Second), 10))
	}
	if hints.Progress != "" {
		h.Set(HeaderProgress, hints.Progress)
	}
	if hints.Daemon != "" {
		h.Set(HeaderDaemon, hints.Daemon)
	}
}

// Parse reads hints from h, ignoring malformed values rather than trusting
// them: a client must fall back to its own bound when the daemon says nothing
// usable.
func Parse(h http.Header) Hints {
	var hints Hints
	if raw := strings.TrimSpace(h.Get(HeaderBudgetRemaining)); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
			hints.HasBudget = true
			hints.BudgetRemaining = maxBudgetRemaining
			if seconds < int64(maxBudgetRemaining/time.Second) {
				hints.BudgetRemaining = time.Duration(seconds) * time.Second
			}
		}
	}
	hints.Progress = token(h, HeaderProgress)
	hints.Daemon = token(h, HeaderDaemon)
	return hints
}

func token(h http.Header, name string) string {
	if raw := strings.TrimSpace(h.Get(name)); len(raw) <= maxTokenLen {
		return raw
	}
	return ""
}
