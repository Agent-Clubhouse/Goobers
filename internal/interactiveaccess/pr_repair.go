package interactiveaccess

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// AuthorizePRRepair checks current read/write authority and exact configured
// repository credential selection without minting a token or nesting policy locks.
func (l *ExecutionLease) AuthorizePRRepair(repository apiv1.InteractiveRepositoryIdentity) error {
	if l == nil {
		return ErrDenied
	}
	if err := l.ctx.Err(); err != nil {
		return err
	}
	for _, action := range []apiv1.InteractiveAction{"repository.read", "pr.repair"} {
		if err := authorize(l.principal, l.gaggle, action); err != nil {
			return err
		}
	}
	_, err := selectSource(l.gaggle, l.service.sources, Target{Kind: "repository", Repository: repository})
	return err
}

// SetPRRepairAvailable advertises installed host repair custody, never permission.
func (s *Service) SetPRRepairAvailable(available bool) { s.prRepairAvailable.Store(available) }
