package startuphint

import "time"

// BudgetGrace is added to a daemon's advertised remaining budget: the budget
// is the daemon's own estimate, and missing it slightly is not "down".
const BudgetGrace = 5 * time.Minute

// Bound is when a client stops waiting for a not-ready daemon.
//
// It starts at the stall window. A daemon that advertises its startup budget
// moves it to the end of that budget plus BudgetGrace, and a daemon whose
// progress token changes earns a fresh stall window from that moment: a slow
// daemon that is visibly advancing is waited for, while one that stops
// advancing is given up on a stall window after its last progress (#6895).
// Neither can move it past the limit.
type Bound struct {
	start, deadline, limit time.Time
	stall                  time.Duration
	progress               string
	// daemon is the process whose progress token is the current baseline;
	// firstDaemon is the only process whose budget extends the bound.
	daemon, firstDaemon string
	seen                bool
}

// NewBound returns a bound starting at now that waits stall without any
// hints and never extends past now+limit. A limit below stall is raised to it.
func NewBound(now time.Time, stall, limit time.Duration) *Bound {
	if limit < stall {
		limit = stall
	}
	return &Bound{start: now, deadline: now.Add(stall), limit: now.Add(limit), stall: stall}
}

// Start is when the wait began.
func (b *Bound) Start() time.Time { return b.start }

// Deadline is when the client should stop waiting, given the hints observed.
func (b *Bound) Deadline() time.Time { return b.deadline }

// Limit is the latest the deadline can ever move to.
func (b *Bound) Limit() time.Time { return b.limit }

// Observe folds one not-ready answer's hints into the bound. The first
// progress token seen from a daemon process is a baseline, not progress, and
// only the first daemon process seen may extend the bound by its budget: a
// crash-looping daemon advertises a fresh budget and a reset progress token
// on every restart, which must not read as advancing. A restart also caps
// the bound at one more stall window, so a loop whose processes each advance
// a little cannot chain extensions. A spent budget extends nothing, so a
// daemon past its own estimate is held only by real progress.
func (b *Bound) Observe(now time.Time, hints Hints) {
	if hints == (Hints{}) {
		return
	}
	if !b.seen {
		b.seen, b.firstDaemon, b.daemon = true, hints.Daemon, hints.Daemon
	}
	next := b.deadline
	if hints.Daemon == b.firstDaemon && hints.HasBudget && hints.BudgetRemaining > 0 {
		next = latest(next, now.Add(hints.BudgetRemaining+BudgetGrace))
	}
	switch {
	case hints.Daemon != b.daemon:
		// A restart: from here on nothing may hold the client past one more
		// stall window, however much the new processes appear to advance.
		b.limit = earliest(b.limit, latest(b.deadline, now.Add(b.stall)))
		b.daemon, b.progress = hints.Daemon, hints.Progress
	case hints.Progress != "" && hints.Progress != b.progress:
		if b.progress != "" {
			next = latest(next, now.Add(b.stall))
		}
		b.progress = hints.Progress
	}
	b.deadline = earliest(next, b.limit)
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
