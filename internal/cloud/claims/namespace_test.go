package claims

import "github.com/goobers/goobers/internal/remediationselect"

// The plane is the control role's claim reader: it satisfies the narrow read the
// shared remediation selection takes, so the scheduler's schedule-demand counter
// is given the plane itself rather than an adapter that could swallow errors.
var _ remediationselect.NamespaceLister = (*Plane)(nil)
