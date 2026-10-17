package interactive

// WithIdleSnapshot validates the current owner and bound policy, then excludes
// turn, child, stop, and reload admission for a short read-only workspace copy.
// capture MUST be bounded pure file copying: it must not execute commands, make
// network requests, mutate the parent workspace, or call any Service method
// (the coordinator mutex is held). Execute copied work only after this method
// returns. The callback result is returned unchanged and no lease is minted.
func (s *Service) WithIdleSnapshot(owner, sessionID string, capture func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkControlLocked(owner, sessionID); err != nil {
		return err
	}
	rec := s.state.Sessions[sessionID]
	pendingLease := func(lease *Lease) bool {
		return lease != nil && (lease.Status == "active" || lease.Status == "policy_fenced" || lease.Status == "custody_pending")
	}
	pending := rec.Session.Status != "idle" || len(s.active[sessionID]) > 0 || rec.Session.CustodyPending || rec.Session.StopFencePending || pendingLease(rec.Session.Lease)
	for _, child := range rec.Session.Children {
		pending = pending || child.Status == "running" || pendingLease(child.Lease)
	}
	if pending {
		return problem(409, "idle_checkpoint_required", "workspace snapshot requires an idle session with no active or unverified writers")
	}
	if capture == nil {
		return problem(400, "invalid", "workspace snapshot capture callback is required")
	}
	return capture()
}
