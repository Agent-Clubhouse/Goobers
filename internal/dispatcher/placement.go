package dispatcher

// placement.go carries the pinned-placement contract: the run-start solve's
// per-stage answer, snapshotted into the run's input (WF-016) and read as
// pure data by whoever drives the run.
//
// It lives HERE rather than in internal/engine (decision 003 ruling 2, plan
// step 3) because the engine is no longer its only consumer. The daemon's
// runner pins the same list into run.yaml and hands one entry to the engine's
// dispatch activity over Temporal; internal/engine keeps a type alias so
// every existing reference — and, critically, every recorded workflow history
// and persisted RunInput — stays byte-identical. internal/engine already
// imports this package for RunnerSpec/Attempt/Report, so the type sits beside
// the eligible-runner set it carries and the dependency arrow does not
// reverse.

// PinnedPlacement is one task's resolved execution placement, pinned into the
// run's input at run start. The zero value never occurs in a pinned list; an
// absent entry (or an empty list — every zero-declaration and local-mode
// instance) leaves the stage on the legacy self path, byte for byte.
//
// The JSON tags are a WIRE CONTRACT: they appear in recorded Temporal
// histories and in persisted run inputs, so an existing history must decode
// into this type unchanged.
type PinnedPlacement struct {
	// Stage is the task name the placement binds to.
	Stage string `json:"stage"`
	// Self marks a placement that resolved to the daemon/worker host: the
	// stage executes through the existing InvokeGoober / RunDeterministic
	// arms, exactly as before this field existed. The dispatcher models the
	// same outcome as Local=true/no-pod, so self stays a first-class
	// placement rather than a special case.
	Self bool `json:"self,omitempty"`
	// Queue is the per-(gaggle × runner-type) task queue the dispatch
	// activity is routed onto (QueueName of the runner SelectRunner picks —
	// D9). Empty inherits the workflow's queue.
	Queue string `json:"queue,omitempty"`
	// Eligible is the solver's eligible runner set for this stage, in
	// inventory order — dispatch consumes eligibility, it never re-derives it
	// (goobernetes-dispatcher.md §2).
	Eligible []RunnerSpec `json:"eligible,omitempty"`
	// LedgerTouching, CPU, Memory, Disk, and Restrictions are the Attempt
	// requirement facts, carried from the run-start solve
	// (runnersolve.StageRequirement) because the workflow cannot recompute
	// them mid-run.
	LedgerTouching bool     `json:"ledgerTouching,omitempty"`
	CPU            string   `json:"cpu,omitempty"`
	Memory         string   `json:"memory,omitempty"`
	Disk           string   `json:"disk,omitempty"`
	Restrictions   []string `json:"restrictions,omitempty"`
}
