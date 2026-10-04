package interactiveaccess

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
