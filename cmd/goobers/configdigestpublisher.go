package main

import "sync/atomic"

// configDigestPublisher holds the config-tree digest the daemon currently has
// in force, so the config-digest plane can answer with the tree that is live
// NOW rather than the one that was live when the HTTP handler was built
// (#4153).
//
// It exists because of an ordering fact: the API handler is constructed well
// before the config reloader that keeps the digest current, so the handler
// cannot hold a reference to the reloader. A shared atomic seeded at startup
// and updated on each applied reload gives both halves one answer without
// either depending on the other's lifetime.
//
// WHY THIS MATTERS FOR THE THING IT REPORTS. The whole point of publishing a
// digest is that a worker can tell its tree has diverged from the daemon's.
// A digest that silently stopped moving would make every worker look
// converged forever — turning the divergence alarm into a source of false
// assurance, which is worse than not having one.
type configDigestPublisher struct {
	digest atomic.Pointer[string]
}

func newConfigDigestPublisher(initial string) *configDigestPublisher {
	p := &configDigestPublisher{}
	p.Set(initial)
	return p
}

// Set records a newly applied config-tree digest.
func (p *configDigestPublisher) Set(digest string) {
	if p == nil {
		return
	}
	p.digest.Store(&digest)
}

// Get returns the digest currently in force, or "" when none has been
// resolved. The route reports an empty digest as unavailable rather than
// serving it: "" is indistinguishable from a real answer to a caller
// comparing strings, and would make every worker conclude it had diverged.
func (p *configDigestPublisher) Get() string {
	if p == nil {
		return ""
	}
	if held := p.digest.Load(); held != nil {
		return *held
	}
	return ""
}
