package interactiveaccess

import (
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// AuthorizeSourceResolve checks only the dedicated marker-resolution permission
// under an existing live session lease. It never acquires another policy lock.
func (l *ExecutionLease) AuthorizeSourceResolve() error {
	if l == nil {
		return ErrDenied
	}
	if err := l.ctx.Err(); err != nil {
		return err
	}
	if err := authorize(l.principal, l.gaggle, "backlog.resolve"); err != nil {
		return err
	}
	_, err := selectSource(l.gaggle, l.service.sources, Target{Kind: "backlog"})
	return err
}

// SetBacklogResolveAvailable advertises an installed session resolver, never an
// action grant. Resolution also requires an installed shared-session runtime.
func (s *Service) SetBacklogResolveAvailable(available bool) {
	s.backlogResolveAvailable.Store(available)
}

func workbenchResolvableBacklog(g *apiv1.Gaggle) bool {
	if g.Spec.Workbench == nil {
		return false
	}
	for _, source := range g.Spec.Workbench.Sources {
		if source.Kind == "backlog" && source.Writes != nil && slices.Contains(source.Writes.Fields, apiv1.WorkbenchField("labels")) {
			return true
		}
	}
	return false
}
