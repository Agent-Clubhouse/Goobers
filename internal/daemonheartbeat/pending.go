package daemonheartbeat

import "sync/atomic"

// PendingUpdate publishes the current "a newer release exists" condition from
// the checker goroutine to the heartbeat goroutine (#4920). An available
// update is a CONDITION, not an event: the one-shot notice scrolls out of
// journalctl behind the per-minute heartbeat, so the heartbeat carries a
// compact clause for as long as the condition holds.
//
// It is an atomic rather than a channel because the heartbeat is a separate
// goroutine that already owns its own stdout writes. Publishing state it reads
// keeps the writer count unchanged; handing it a second channel to render from
// would not.
type PendingUpdate struct{ version atomic.Pointer[string] }

// Set records the available version, or clears the condition when empty.
func (p *PendingUpdate) Set(version string) {
	if version == "" {
		p.version.Store(nil)
		return
	}
	p.version.Store(&version)
}

// Clause renders the heartbeat suffix, or "" for a nil holder or current build. The
// clause is deliberately terse: the actionable sentence naming the command is
// the once-per-version notice, and repeating it every minute would be noise.
func (p *PendingUpdate) Clause() string {
	if p == nil {
		return ""
	}
	version := p.version.Load()
	if version == nil {
		return ""
	}
	return "; update " + *version + " available"
}
