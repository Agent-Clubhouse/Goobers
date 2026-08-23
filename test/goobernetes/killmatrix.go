package goobernetes

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry"
)

// KillMatrixObserver is S6's named observer (goobernetes-smoke.md §4 S6):
// "per injection — the evidence bundle's injection record (what was killed,
// when), the interrupted attempt's attemptClass: infra journal entry with a
// typed infra* error class, the successor attempt's fresh runner.* identity,
// and the run's successful terminal event."
const KillMatrixObserver = "CellInjectionRecord (D5) + StageAttempt.Class==\"infra\" + telemetry.ClassifyError(...).InfraFault() + successor Placement.Pod + run terminal phase"

// StageClass is the kill matrix's first axis (goobernetes-smoke.md §2/S6):
// the three stage classes one smoke run's kill matrix covers. This is a
// closed, smoke-doc-defined set — it does NOT name real workflow stages
// (which stage in a live topology plays "the agentic one" is topology
// content, filled in when the YAML agent's stage set exists).
type StageClass string

// The three stage classes S6's kill matrix covers.
const (
	StageClassBuiltin StageClass = "builtin"
	StageClassAgentic StageClass = "agentic"
	StageClassLocalCI StageClass = "local-ci"
)

// AllStageClasses is S6's stage-class axis, in the smoke doc's own order:
// "(a) a builtin stage, (b) an agentic stage, (c) local-ci."
func AllStageClasses() []StageClass {
	return []StageClass{StageClassBuiltin, StageClassAgentic, StageClassLocalCI}
}

// FailureKind is the kill matrix's second axis: "pod-kill and node-kill."
type FailureKind string

// The two failure kinds S6's kill matrix covers.
const (
	FailureKindPodKill  FailureKind = "pod-kill"
	FailureKindNodeKill FailureKind = "node-kill"
)

// AllFailureKinds is S6's failure-kind axis.
func AllFailureKinds() []FailureKind { return []FailureKind{FailureKindPodKill, FailureKindNodeKill} }

// KillMatrixCell identifies one of the six required injections (3 stage
// classes x 2 failure kinds — goobernetes-smoke.md §4 S6: "Six injections").
type KillMatrixCell struct {
	Stage   StageClass
	Failure FailureKind
}

// String renders a cell as "<stage>/<failure>" for diagnostics and bundle
// keys.
func (c KillMatrixCell) String() string { return fmt.Sprintf("%s/%s", c.Stage, c.Failure) }

// KillMatrix returns the fixed six-cell structure S6 requires, in a
// deterministic order (stage-class major, failure-kind minor). It is a pure
// enumeration — no cluster, no topology — so both the fixture tests here and
// a live driver iterate identically.
func KillMatrix() []KillMatrixCell {
	cells := make([]KillMatrixCell, 0, len(AllStageClasses())*len(AllFailureKinds()))
	for _, stage := range AllStageClasses() {
		for _, failure := range AllFailureKinds() {
			cells = append(cells, KillMatrixCell{Stage: stage, Failure: failure})
		}
	}
	return cells
}

// CellDriver is the SEAM a live driver implements once the topology exists:
// executing one kill-matrix cell against a real cluster (deleting a pod,
// draining/killing a node) mid-attempt and returning the resulting evidence.
// TOPOLOGY-PENDING (#3517): no implementation exists in this package. It is
// filled in once the YAML agent's smoke-workflow stage set and infra's
// derived runner inventory exist — until then, StaticCellDriver (below)
// lets the rest of this package's machinery (ClassifyCellResult, the
// evidence bundle) be exercised against fixtures.
//
// goobernetes-smoke.md §8 open point 2 leaves the injection MECHANICS open
// ("chaos tool vs. scripted delete pod / node drain") — this interface is
// deliberately silent on which; D5's only requirement is that "each
// injection is itself recorded," which Inject's return value satisfies.
type CellDriver interface {
	// Inject performs one kill-matrix cell's injection against whatever
	// stage attempt of runID is currently executing cell.Stage, and returns
	// the recorded evidence once the run has settled (retried and completed,
	// or given up). It is the live driver's job to identify which running
	// attempt matches cell.Stage on the actual topology — this package has
	// no notion of which real stage name plays which StageClass role.
	Inject(ctx context.Context, runID string, cell KillMatrixCell) (CellInjectionRecord, error)
}

// CellInjectionRecord captures one kill-matrix cell's evidence (D5: "each
// injection is itself recorded in the evidence bundle") — what was killed,
// when, and the before/after attempt state ClassifyCellResult needs.
type CellInjectionRecord struct {
	Cell  KillMatrixCell `json:"cell"`
	RunID string         `json:"runId"`
	// InjectedAt and InjectedTarget record D5's "what was killed, when" —
	// InjectedTarget is a pod name or node name depending on Cell.Failure.
	InjectedAt     time.Time `json:"injectedAt"`
	InjectedTarget string    `json:"injectedTarget"`
	// InterruptedAttempt is the attempt that was executing when the
	// injection landed. SuccessorAttempt is the attempt (if any) that
	// resumed the stage afterward.
	InterruptedAttempt readservice.StageAttempt  `json:"interruptedAttempt"`
	SuccessorAttempt   *readservice.StageAttempt `json:"successorAttempt,omitempty"`
	// RunCompletedSuccessfully reports the run's eventual terminal phase.
	RunCompletedSuccessfully bool `json:"runCompletedSuccessfully"`
}

// ClassifyCellResult applies S6's pass/fail rule to one recorded injection:
//
//   - the interrupted attempt must journal attemptClass: infra with a typed
//     infra* error class (never a policy/work failure charging Task.Retry or
//     the failure-streak breaker — "the #3361 regression class");
//   - the successor attempt must exist and carry a fresh pod identity (S1);
//   - the run must complete successfully.
//
// telemetry.ClassifyError/.InfraFault() (internal/telemetry/errorclass.go)
// is the real, already-shipped infra-fault classifier this reuses rather
// than re-deriving "is this code infra-shaped" — TEL-012/#22's own
// taxonomy, consumed exactly as the failure-streak breaker and
// success-rate denominator already do (#3364).
func ClassifyCellResult(record CellInjectionRecord) AssertionResult {
	if record.InjectedTarget == "" || record.InjectedAt.IsZero() {
		return invalid(fmt.Sprintf("cell %s: no injection recorded (D5 requires every injection be recorded)", record.Cell), record)
	}

	interrupted := record.InterruptedAttempt
	if interrupted.Class != string(journal.AttemptInfra) {
		return classify("", false,
			fmt.Sprintf("cell %s: interrupted attempt classified as %q, not %q — an infra kill journaled as a policy/work failure charges Task.Retry or the failure-streak breaker (the #3361 regression class)",
				record.Cell, interrupted.Class, journal.AttemptInfra),
			nil, record)
	}
	if interrupted.Error == nil || interrupted.Error.Code == "" {
		return classify("", false, fmt.Sprintf("cell %s: interrupted attempt carries attemptClass infra but no typed error code", record.Cell), nil, record)
	}
	if class := telemetry.ClassifyError(interrupted.Error.Code); !class.InfraFault() {
		return classify("", false,
			fmt.Sprintf("cell %s: interrupted attempt's error code %q classifies as %q, not an infra fault", record.Cell, interrupted.Error.Code, class),
			nil, record)
	}

	if record.SuccessorAttempt == nil {
		return classify("", false, fmt.Sprintf("cell %s: no successor attempt recorded — the retry never ran in a fresh pod (S1)", record.Cell), nil, record)
	}
	successor := *record.SuccessorAttempt
	if successor.Placement == nil || successor.Placement.Pod == "" {
		return invalid(fmt.Sprintf("cell %s: successor attempt carries no placement provenance", record.Cell), record)
	}
	if interrupted.Placement != nil && interrupted.Placement.Pod == successor.Placement.Pod {
		return classify("", false, fmt.Sprintf("cell %s: successor attempt reused the interrupted attempt's pod %q", record.Cell, successor.Placement.Pod), nil, record)
	}

	if !record.RunCompletedSuccessfully {
		return classify("", false, fmt.Sprintf("cell %s: run did not complete successfully after the kill/retry cycle", record.Cell), nil, record)
	}

	return classify("", true, "", record, nil)
}

// StaticCellDriver is a CellDriver over a fixed, pre-recorded map of results
// — a fixture double for RunKillMatrix, standing in for the real cluster
// driver until the topology exists. It never touches a cluster; every
// record it returns was supplied at construction. Tests use it to prove
// RunKillMatrix's iteration/aggregation contract without a live driver.
type StaticCellDriver struct {
	Records map[KillMatrixCell]CellInjectionRecord
	// Err, if set, names the first cell (by KillMatrix order) at which
	// Inject should fail, simulating a live driver's own injection failure.
	Err         error
	FailingAt   KillMatrixCell
	failEnabled bool
}

// NewStaticCellDriver builds a StaticCellDriver from a complete result map.
func NewStaticCellDriver(records map[KillMatrixCell]CellInjectionRecord) *StaticCellDriver {
	return &StaticCellDriver{Records: records}
}

// FailAt configures the driver to return err the first time cell is
// injected, regardless of any record present for it.
func (d *StaticCellDriver) FailAt(cell KillMatrixCell, err error) {
	d.FailingAt = cell
	d.Err = err
	d.failEnabled = true
}

// Inject implements CellDriver by looking cell up in Records.
func (d *StaticCellDriver) Inject(_ context.Context, runID string, cell KillMatrixCell) (CellInjectionRecord, error) {
	if d.failEnabled && cell == d.FailingAt {
		return CellInjectionRecord{}, d.Err
	}
	record, ok := d.Records[cell]
	if !ok {
		return CellInjectionRecord{}, fmt.Errorf("goobernetes: StaticCellDriver has no record for cell %s", cell)
	}
	record.RunID = runID
	record.Cell = cell
	return record, nil
}

// RunKillMatrix drives every KillMatrix cell for runID through driver,
// returning one CellInjectionRecord per cell in KillMatrix order. It stops
// and returns the error on the first Inject failure — S6 requires ALL SIX
// cells to succeed in one procedure (goobernetes-smoke.md §4: "All must pass
// in one procedure"), so a live caller re-runs the whole matrix rather than
// resuming a partial one.
func RunKillMatrix(ctx context.Context, driver CellDriver, runID string) ([]CellInjectionRecord, error) {
	if driver == nil {
		return nil, fmt.Errorf("goobernetes: RunKillMatrix requires a non-nil CellDriver (topology-pending: no implementation exists yet, #3517)")
	}
	cells := KillMatrix()
	records := make([]CellInjectionRecord, 0, len(cells))
	for _, cell := range cells {
		record, err := driver.Inject(ctx, runID, cell)
		if err != nil {
			return records, fmt.Errorf("goobernetes: kill-matrix cell %s: %w", cell, err)
		}
		records = append(records, record)
	}
	return records, nil
}
