package pgroles

// EnsureRoleForTest exposes role creation so the Entra path can be exercised
// on a test-owned role, without touching the shared runtime roles.
var EnsureRoleForTest = ensureRole
