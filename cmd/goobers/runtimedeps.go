package main

// runtimeDeps holds the factories the daemon uses to reach external systems
// (#2435). It replaces package-level function vars that tests reassigned and
// restored: such vars are shared process state, so any test touching one
// cannot run in parallel and a missed restore leaks into later tests. The
// value is passed by copy, so a test that copies productionRuntimeDeps(),
// replaces the field it fakes, and hands that copy to the constructor under
// test changes nothing any other test sees.
//
// The target is one value built at the daemon's composition root and threaded
// through scheduler and runner construction. Until that root is threaded,
// production entry points such as buildOpenPRRefresher build it per call.
// Seam families move here one at a time; #2435 tracks the inventory and the
// order.
type runtimeDeps struct {
	openPRListers openPRListerDeps
}

// productionRuntimeDeps is the production composition of every runtimeDeps
// family.
func productionRuntimeDeps() runtimeDeps {
	return runtimeDeps{openPRListers: productionOpenPRListerDeps()}
}
