package journal

// placementpin.go carries the run.yaml spelling of a run's pinned stage
// placements (decision 003 ruling 1, plan step 6): the daemon's runner pins
// the run-start solve's per-stage answer beside RunControls so a resumed run
// restores the SAME pins rather than re-solving against an inventory that may
// have changed underneath it (accept-and-pin, D4).
//
// These are MIRROR types of internal/dispatcher.PinnedPlacement and
// dispatcher.RunnerSpec, field for field and tag for tag, rather than the
// types themselves: the dispatcher already depends on this package
// (transitively, and on localscheduler, whose tests import the runner), so
// neither the journal nor the runner can import it without a cycle. The
// runner carries THIS spelling end to end — StartInput, run.yaml, the seam
// request — and dispatcher.PinnedPlacementFromJournal / PinnedPlacement.
// Journal are the one conversion pair, kept honest by
// TestPinnedPlacementJournalMirror in internal/dispatcher, which round-trips
// every field through JSON in both directions and fails on any tag or field
// drift.
//
// Absent (nil, omitted from run.yaml) is the ONLY value a journal written
// before this field existed can carry, and it means what it always meant: no
// placement was pinned, every stage takes the self path. A zero-declaration
// or local-mode instance never pins, so its run.yaml stays byte-identical.

// PinnedPlacement is one stage's resolved execution placement as pinned into
// run.yaml. JSON tags match dispatcher.PinnedPlacement exactly.
type PinnedPlacement struct {
	// Stage is the task (or, under name-keyed pinning, gate) name.
	Stage string `json:"stage"`
	// Self marks a placement that resolved to the daemon's own host: the
	// stage executes through the in-process arms exactly as before pins
	// existed.
	Self bool `json:"self,omitempty"`
	// Queue is the dispatch task queue the placement routes onto.
	Queue string `json:"queue,omitempty"`
	// Eligible is the solver's eligible runner set, in inventory order.
	Eligible []PinnedRunner `json:"eligible,omitempty"`
	// LedgerTouching, CPU, Memory, Disk, and Restrictions are the attempt
	// requirement facts carried from the run-start solve.
	LedgerTouching bool     `json:"ledgerTouching,omitempty"`
	CPU            string   `json:"cpu,omitempty"`
	Memory         string   `json:"memory,omitempty"`
	Disk           string   `json:"disk,omitempty"`
	Restrictions   []string `json:"restrictions,omitempty"`
}

// PinnedRunner is one eligible runner as pinned into run.yaml. JSON tags
// match dispatcher.RunnerSpec exactly; HostKind is the plain string spelling
// of instance.RunnerHostKind for the same import-direction reason.
type PinnedRunner struct {
	Name         string   `json:"name"`
	OS           string   `json:"os,omitempty"`
	HostKind     string   `json:"hostKind"`
	Host         string   `json:"host"`
	CPU          string   `json:"cpu,omitempty"`
	Memory       string   `json:"memory,omitempty"`
	Disk         string   `json:"disk,omitempty"`
	Restrictions []string `json:"restrictions,omitempty"`
}
