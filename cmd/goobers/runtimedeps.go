package main

// runtimeDeps holds the factories the daemon's scheduler and runners use to
// reach external systems (#2435). It replaces package-level function vars
// that tests reassigned and restored: such vars are shared process state, so
// any test touching one cannot run in parallel and a missed restore leaks into
// later tests. A runtimeDeps value is built once with production defaults and
// passed down by copy; its fields are unexported, so nothing downstream can
// swap a factory after construction. A test copies productionRuntimeDeps(),
// replaces the field it fakes, and passes that copy to the constructor under
// test.
//
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
