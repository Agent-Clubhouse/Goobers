package interactive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/secretpattern"
)

// prepareSurfaces validates before copying so explicit empty allowlists cannot
// become nil through JSON omitempty. Nil retains legacy host-level policy.
func prepareSurfaces(input []Surface) ([]Surface, error) {
	seen := map[string]bool{}
	for _, surface := range input {
		if surface.ID == "" || seen[surface.ID] || (surface.Mode != "helper" && surface.Mode != "workbench") || surface.MaxChildren < 0 || surface.MaxChildren > 8 {
			return nil, errors.New("invalid or duplicate interactive surface")
		}
		seen[surface.ID] = true
		if surface.MaxTurnSeconds < 0 || surface.MaxTurnSeconds > 86400 || surface.LeaseSeconds < 0 || surface.LeaseSeconds > 86400 || surface.MaxActive < 0 || surface.MaxActivePerOwner < 0 {
			return nil, fmt.Errorf("surface %s has invalid execution limits", surface.ID)
		}
		if surface.MaxActive > 0 && surface.MaxActivePerOwner > surface.MaxActive {
			return nil, fmt.Errorf("surface %s owner limit exceeds surface capacity", surface.ID)
		}
		if surface.ExecutionModes != nil && len(surface.ExecutionModes) == 0 {
			return nil, fmt.Errorf("surface %s requires at least one execution mode", surface.ID)
		}
		modes := map[string]bool{}
		for _, mode := range surface.ExecutionModes {
			if (mode != ExecutionModeCodexLocal && mode != ExecutionModeLocalTools) || modes[mode] {
				return nil, fmt.Errorf("surface %s has invalid or duplicate execution mode", surface.ID)
			}
			modes[mode] = true
		}
		if surface.DefaultExecutionMode != "" && ((surface.DefaultExecutionMode != ExecutionModeCodexLocal && surface.DefaultExecutionMode != ExecutionModeLocalTools) || !surfaceAllowsMode(surface, surface.DefaultExecutionMode)) {
			return nil, fmt.Errorf("surface %s default execution mode is not allowed", surface.ID)
		}
		for _, entries := range [][]string{surface.Tools, surface.Workflows, surface.AuthorityRefs, surface.ChildCommandCapabilities} {
			entriesSeen := map[string]bool{}
			for _, entry := range entries {
				if strings.TrimSpace(entry) != entry || entry == "" || entriesSeen[entry] {
					return nil, fmt.Errorf("surface %s has empty or duplicate policy reference", surface.ID)
				}
				entriesSeen[entry] = true
			}
		}
	}
	result := clone(input)
	scrubber := secretpattern.NewScrubber()
	for i := range result {
		result[i].Instructions = string(scrubber.Scrub([]byte(result[i].Instructions)))
	}
	return result, nil
}

func surfaceAllowsMode(surface Surface, mode string) bool {
	if surface.ExecutionModes == nil {
		return true
	}
	for _, available := range surface.ExecutionModes {
		if mode == available {
			return true
		}
	}
	return false
}

// Global ceilings remain an outer bound; zero surface values preserve legacy
// defaults. A surface can reduce these ceilings, never expand host capacity.
func boundedDuration(seconds int, fallback time.Duration) time.Duration {
	if seconds > 0 && time.Duration(seconds)*time.Second < fallback {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}
func boundedCount(value, fallback int) int {
	if value > 0 && value < fallback {
		return value
	}
	return fallback
}
func (s *Service) bindSurfaceRequest(req *ExecutionRequest, surface Surface) {
	req.ConfigRevision = surface.ConfigRevision
	req.Gaggle, req.Goober, req.Model, req.Harness = surface.Gaggle, surface.Goober, surface.Model, surface.Harness
	req.Tools = append([]string(nil), surface.Tools...)
	req.Workflows = append([]string(nil), surface.Workflows...)
	req.AuthorityRefs = append([]string(nil), surface.AuthorityRefs...)
	req.ChildCommandCapabilities = append([]string(nil), surface.ChildCommandCapabilities...)
	req.leaseDuration = boundedDuration(surface.LeaseSeconds, s.opts.LeaseDuration)
	req.maxTurnDuration = boundedDuration(surface.MaxTurnSeconds, s.opts.MaxTurnDuration)
	req.turnExpiresAt = s.opts.Now().Add(req.maxTurnDuration)
	if req.ChildID != "" {
		// The bounded command list is only input to the native child validator;
		// it is not a tool grant. Child identity cannot recursively propose work.
		req.Tools, req.Workflows, req.AuthorityRefs = nil, nil, nil
	}
}

func (s *Service) checkCapacity(surface Surface, owner, sessionID string, resuming bool) error {
	total, owned, surfaceTotal, surfaceOwned := 0, 0, 0, 0
	count := func(actor, surfaceID string) {
		total++
		if actor == owner {
			owned++
		}
		if surfaceID == surface.ID {
			surfaceTotal++
			if actor == owner {
				surfaceOwned++
			}
		}
	}
	activeLeases := map[string]bool{}
	for _, runs := range s.active {
		for _, run := range runs {
			count(run.request.Owner, run.request.SurfaceID)
			activeLeases[run.request.LeaseID] = true
		}
	}
	for id, record := range s.state.Sessions {
		if resuming && id == sessionID {
			continue
		}
		leases := []*Lease{record.Session.Lease}
		for _, child := range record.Session.Children {
			leases = append(leases, child.Lease)
		}
		for _, lease := range leases {
			if lease != nil && (lease.Status == "custody_pending" || lease.Status == "policy_fenced") && !activeLeases[lease.LeaseID] {
				count(record.Session.Owner, record.Session.SurfaceID)
			}
		}
	}
	if total >= s.opts.MaxActive || owned >= s.opts.MaxActivePerOwner || surfaceTotal >= boundedCount(surface.MaxActive, s.opts.MaxActive) || surfaceOwned >= boundedCount(surface.MaxActivePerOwner, s.opts.MaxActivePerOwner) {
		return problem(409, "capacity", "interactive execution budget is occupied; retry when active work finishes")
	}
	return nil
}

// SurfaceForSession returns a copy of the current bound policy after admission
// checks. It allows a running parent, but never a changed or fenced session.
func (s *Service) SurfaceForSession(owner, sessionID string) (Surface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkControlLocked(owner, sessionID); err != nil {
		return Surface{}, err
	}
	surface, _ := s.surface(s.state.Sessions[sessionID].Session.SurfaceID)
	return clone(surface), nil
}

// ReloadSurfaces keeps the last configuration on validation, persistence, or
// external-fence failure. Once durable fencing starts it is not rolled back:
// affected sessions remain recovery_required even if an external hook fails.
// The caller must publish its corresponding catalog only after a nil result.
func (s *Service) ReloadSurfaces(input []Surface) error {
	next, err := prepareSurfaces(input)
	if err != nil {
		return err
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	s.mu.Lock()
	if s.closed || s.fatal != nil {
		s.mu.Unlock()
		return problem(503, "unavailable", "interactive coordinator unavailable")
	}
	byID := map[string]Surface{}
	for _, surface := range next {
		byID[surface.ID] = surface
	}
	affected := map[string]string{}
	for sessionID, rec := range s.state.Sessions {
		surface, exists := byID[rec.Session.SurfaceID]
		// ConfigDigest intentionally remains the original binding. Idle sessions
		// may own native Helper runs outside s.active and need the same fence.
		if (!exists || !surface.Enabled || rec.Session.ConfigDigest != digest(surface)) && rec.Session.Status != "stopped" {
			affected[sessionID] = rec.Session.Owner
		}
	}
	err = s.mutate(func(state *diskState) error {
		for sessionID := range affected {
			rec := state.Sessions[sessionID]
			rec.Session.Fence++
			rec.Session.Status = "recovery_required"
			rec.Session.RecoveryReason = "Surface policy changed; prior work is fenced. Create a new session for the new configuration."
			rec.Session.StopFencePending = s.opts.OnStop != nil
			for _, run := range s.active[sessionID] {
				if lease := leaseFor(rec, run.request.ChildID); lease != nil {
					lease.Status = "policy_fenced"
				}
			}
			rec.Session.CustodyPending = rec.Session.CustodyPending || len(s.active[sessionID]) > 0
			for i := range rec.Session.Turns {
				if rec.Session.Turns[i].Status == "running" {
					rec.Session.Turns[i].Status = "interrupted"
				}
			}
			for i := range rec.Session.Children {
				if rec.Session.Children[i].Status == "running" {
					rec.Session.Children[i].Status = "uncertain"
				}
			}
			appendEvent(rec, "recovery.required", rec.Session.ActiveTurnID, "", map[string]any{"reason": rec.Session.RecoveryReason, "fence": rec.Session.Fence, "custodyPending": rec.Session.CustodyPending, "policyChanged": true})
		}
		return nil
	})
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.reloading = true
	for sessionID := range affected {
		for _, run := range s.active[sessionID] {
			run.cancel()
		}
	}
	s.mu.Unlock()
	failures := map[string]error{}
	if s.opts.OnStop != nil {
		for sessionID, owner := range affected {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			failures[sessionID] = s.opts.OnStop(ctx, owner, sessionID)
			cancel()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { s.reloading = false }()
	var hookErrors []error
	for sessionID, failure := range failures {
		if failure != nil {
			hookErrors = append(hookErrors, fmt.Errorf("session %s authority fence: %s", sessionID, s.clean(failure.Error())))
		}
	}
	if len(failures) > 0 {
		if err := s.mutate(func(state *diskState) error {
			for sessionID, failure := range failures {
				rec := state.Sessions[sessionID]
				rec.Session.StopFencePending = failure != nil
				if failure != nil {
					appendEvent(rec, "auth.fence_pending", "", "", map[string]any{"reason": s.clean(failure.Error())})
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if len(hookErrors) > 0 {
		return errors.Join(hookErrors...)
	}
	if s.closed {
		return problem(503, "unavailable", "coordinator closed during surface reload")
	}
	s.opts.Surfaces = next
	select {
	case s.policyWake <- struct{}{}:
	default:
	}
	return nil
}
