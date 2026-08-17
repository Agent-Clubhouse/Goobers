package readmodel

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// Disposition values for RunRow.Disposition (§5.3).
//
// Only two of the three reserved enum values are ever written here.
// DispositionProduced is deliberately absent: defining "did this run produce
// something" for every workflow shape is #1429's contract, not this
// projector's to guess at. DispositionUnknown is the safe default for
// everything this projector cannot classify — including a real productive
// run — until #1429 lands.
const (
	DispositionUnknown = "unknown"
	// DispositionNoWork marks a run that touched exactly one stage and that
	// stage's terminal status was apiv1.ResultNoWork (#2188). Expressed as a
	// bare string rather than importing api/v1alpha1: event.Status is already
	// a bare string by the time it reaches the projector, and this package
	// has no other reason to depend on the API layer.
	DispositionNoWork = "no-work"
)

// Projection: journal events to a run row.
//
// §10 requires projector units to be "a pure function (prevRow, events) →
// nextRow", and §14.9 makes determinism a tested property — rebuilding from the
// same journals must produce byte-identical canonical rows.
//
// Purity is not decoration here. It is what makes three separate things
// possible: incremental projection (apply only the tail past last_seq),
// rebuild-equals-incremental (the same function over all events), and a
// differential oracle against the journal-derived path (§14.7). A projection
// that reached for the clock or the filesystem would satisfy none of them.
//
// # What is deliberately absent
//
// No duration. It is now-relative for a running run, so storing it would freeze
// a quiet in-flight run's age at projection time (§5.3). The read contract
// computes it at query time from started_at and finished_at.
//
// No filesystem access. state.json is NOT consulted, even though
// summarizeRunForStage reads it to refine current_stage for a running run. The
// journal is the authoritative record (§3.2) and state.json can lag a
// crash-fsynced run.finished; a projection that mixed the two would be
// non-deterministic in exactly the case that matters. The projector's
// current_stage therefore comes from events alone.

// RunRow is one projected run — the complete row a list page is answered from,
// with no journal open.
type RunRow struct {
	RunID           string
	Gaggle          string
	Workflow        string
	WorkflowVersion int
	WorkflowDigest  string
	GooberDigest    string
	TriggerKind     string
	TriggerRef      string

	Phase        journal.RunPhase
	Terminal     bool
	CurrentStage string
	StartedAt    time.Time
	FinishedAt   *time.Time
	LastActivity time.Time
	LastSeq      uint64

	RepassCount      int
	RetryCount       int
	PolicyRetryCount int
	InfraRetryCount  int

	OutcomeVerdict string
	OutcomeTarget  string

	// Disposition is the reserved semantic-work-disposition column (§5.3):
	// 'no-work' when this run touched exactly one stage and that stage's
	// terminal status was no-work (#2188), 'unknown' otherwise. It never
	// claims 'produced' — that half of the enum, and the rest of the
	// contract, is #1429/#1439's to define; this only ever asserts the one
	// classification the existing no-work signal already answers cleanly.
	Disposition string

	// Stages is every stage or gate the run has touched, sorted. It backs the
	// stage filter without a join (§5.7's run-level rollup principle).
	Stages []string

	// Run-level measurement rollups: the OR across this run's stage rows
	// (#1782). They exist so `population=Y` with no stage is a direct seek on a
	// partial index over run recency, rather than a correlated EXISTS against
	// run_stage -- which is a residual predicate, and residual predicates are
	// exactly what §5.7's closed set refuses.
	AnyTokenMeasured   bool
	AnyPremiumMeasured bool
	AnyCostMeasured    bool
	AnyRetryWaste      bool

	Operator OperatorFacts

	// scratch holds nullable columns between Scan and decode. Unexported and
	// cleared by finishScan, so it never escapes into a returned row.
	scratch *nullables
}

// OperatorFacts are journal-derived facts needed by operator run summaries.
// They are stored with the run row so bounded list reads never reopen journals.
type OperatorFacts struct {
	IssueNumber           string
	IssueTitle            string
	LastHeartbeatAt       *time.Time
	PullRequest           *journal.ExternalRef
	ProviderClaimRecorded bool
	LatestError           *journal.ErrorDetail
	ReviewVerdict         string
	ReviewRationale       string
	ReviewProblem         string
	PROpenerStage         string
}

// StageRow is one projected (run, stage) pair.
type StageRow struct {
	RunID            string
	Stage            string
	Attempts         int
	LastStatus       string
	LastAttemptClass string
	StartedAt        *time.Time
	FinishedAt       *time.Time
	HadSuccess       bool
	HadFailure       bool
	HadOther         bool
	openAttempts     int

	// The four measurement flags (#1782, §5.7).
	//
	// Not computed by ProjectRun: the journal does not carry token counts, cost,
	// or premium-request figures at all -- those exist only in the telemetry
	// rollup. They arrive via ApplyMeasurement and are carried across incremental
	// projections by carryStages, so a tail projected while telemetry is
	// unavailable preserves them rather than silently clearing them.
	TokenMeasured   bool
	PremiumMeasured bool
	CostMeasured    bool
	RetryWaste      bool
}

// NodeRow is one graph node visited by a run. Unlike StageRow it includes gates.
// Identity is reserved for a node-specific prompt or tool identity when the
// journal carries one; the run-wide goober digest must not be used here.
type NodeRow struct {
	RunID              string
	Kind               string
	Name               string
	Identity           string
	Attempts           int
	RetryWasteAttempts int

	branchAttempts map[int]int
	gateOpen       map[int]bool
	humanVisit     map[int]bool
	humanRequested map[int]bool
}

// StageMeasurement is what the telemetry rollup knows about one stage that the
// journal does not.
//
// Each field is an EXISTENCE predicate over that stage's attempts, matching what
// the list filter asks: "does this stage have any attempt with cost recorded",
// not "what did it cost". Storing the predicate rather than the figure is what
// keeps this a filter index instead of a second copy of telemetry.
type StageMeasurement struct {
	Stage           string
	TokenMeasured   bool
	PremiumMeasured bool
	CostMeasured    bool
	RetryWaste      bool
}

// ApplyMeasurement merges telemetry-derived measurement into a projection.
//
// Separate from ProjectRun rather than folded into it, because ProjectRun is a
// pure function of (identity, prev, events) and §10 requires it to stay that
// way. Measurement comes from a different store on a different clock; making
// ProjectRun query it would make the fold impure and untestable in isolation.
//
// # Both grains are set here
//
// The per-stage flags answer `stage=X&population=Y`. The run-level rollups are
// the OR across stages and answer `population=Y` alone. Deriving the rollup here
// rather than in SQL is what guarantees the two cannot disagree -- a run flagged
// at run level always has a stage that justifies it.
//
// Passing nil is meaningful and distinct from passing an empty slice: nil means
// "telemetry had nothing to say", which leaves carried-forward flags intact. An
// empty slice means "telemetry says this run has no measured stages", which
// clears them.
func (p *Projection) ApplyMeasurement(measurements []StageMeasurement) {
	if measurements == nil {
		// Still recompute the rollup from whatever the stage rows carry, so the
		// two grains stay consistent even on a pass that learned nothing new.
		p.rollUpMeasurement()
		return
	}
	byStage := make(map[string]StageMeasurement, len(measurements))
	for _, m := range measurements {
		byStage[m.Stage] = m
	}
	for i := range p.Stages {
		m, ok := byStage[p.Stages[i].Stage]
		if !ok {
			// A projected stage telemetry has no attempts for. Clearing rather
			// than carrying: the measurement source spoke, and it did not name
			// this stage.
			p.Stages[i].TokenMeasured = false
			p.Stages[i].PremiumMeasured = false
			p.Stages[i].CostMeasured = false
			p.Stages[i].RetryWaste = false
			continue
		}
		p.Stages[i].TokenMeasured = m.TokenMeasured
		p.Stages[i].PremiumMeasured = m.PremiumMeasured
		p.Stages[i].CostMeasured = m.CostMeasured
		p.Stages[i].RetryWaste = m.RetryWaste
	}
	p.rollUpMeasurement()
}

// rollUpMeasurement recomputes the run-level flags as the OR across stage rows.
func (p *Projection) rollUpMeasurement() {
	p.Run.AnyTokenMeasured = false
	p.Run.AnyPremiumMeasured = false
	p.Run.AnyCostMeasured = false
	p.Run.AnyRetryWaste = false
	for _, stage := range p.Stages {
		p.Run.AnyTokenMeasured = p.Run.AnyTokenMeasured || stage.TokenMeasured
		p.Run.AnyPremiumMeasured = p.Run.AnyPremiumMeasured || stage.PremiumMeasured
		p.Run.AnyCostMeasured = p.Run.AnyCostMeasured || stage.CostMeasured
		p.Run.AnyRetryWaste = p.Run.AnyRetryWaste || stage.RetryWaste
	}
}

// Projection is the full result of projecting a run.
type Projection struct {
	Run    RunRow
	Stages []StageRow
	Nodes  []NodeRow
}

// ProjectRun folds a run's identity and events into a projection.
//
// prev is the FULL previous projection — run row and stage rows — or the zero
// value for a first projection. events must be the tail beyond
// prev.Run.LastSeq, in sequence order, or the complete history when prev is
// zero. Both produce the same result for the same total event set, which is what
// makes rebuild and incremental projection interchangeable.
//
// prev carries the stage rows, not just the run row, and that is not an
// ergonomic choice. Stage accumulators (attempt counts, first started_at) are
// per-run state that spans batches: an earlier version took only the run row,
// and a run projected incrementally had its stage attempt counts reset on every
// tail — while the same run projected in one pass was correct. The
// incremental-equals-whole test is what surfaced it, which is precisely why that
// test enumerates every split point rather than checking one.
func ProjectRun(identity journal.RunIdentity, prev Projection, events []journal.Event) Projection {
	row := prev.Run
	row.RunID = identity.RunID
	row.Gaggle = identity.Gaggle
	row.Workflow = identity.Workflow
	row.WorkflowVersion = identity.WorkflowVersion
	row.WorkflowDigest = identity.WorkflowDigest
	row.TriggerKind = string(identity.Trigger.Kind)
	row.TriggerRef = identity.Trigger.Ref
	row.StartedAt = identity.StartedAt
	if identity.Trigger.Kind == journal.TriggerItem && row.Operator.IssueTitle == "" {
		row.Operator.IssueNumber = identity.Trigger.Ref
	}

	// A run with no terminal event is running. Seeding here rather than only on
	// an event means a first projection of an in-flight run is correct, and a
	// re-projection of a previously-running run does not lose its phase.
	if row.Phase == "" {
		row.Phase = journal.PhaseRunning
	}

	seenStages := stageSet(prev.Run.Stages)
	stages := carryStages(prev.Stages)
	nodes := carryNodes(prev.Nodes)

	for _, event := range events {
		if event.Seq > row.LastSeq {
			row.LastSeq = event.Seq
			row.LastActivity = event.Time
		}
		// An event from a schema this build does not know still advances the
		// sequence and activity time — it happened — but its semantics are not
		// guessed at. Mirrors summarizeRunForStage.
		if !event.KnownSchema() {
			continue
		}
		if event.Stage != "" {
			seenStages[event.Stage] = struct{}{}
		}
		if event.Gate != "" {
			seenStages[event.Gate] = struct{}{}
		}
		if event.Error != nil {
			detail := *event.Error
			row.Operator.LatestError = &detail
		}

		switch event.Type {
		case journal.EventStageHeartbeat:
			if row.Operator.LastHeartbeatAt == nil || event.Time.After(*row.Operator.LastHeartbeatAt) {
				at := event.Time
				row.Operator.LastHeartbeatAt = &at
			}
		case journal.EventRunnerAnnotation:
			if queue, ok := RunnerQueueStatus(event); ok {
				row.CurrentStage = queue
			}
			if suggestion, ok := RunnerResetSuggestion(event); ok {
				row.CurrentStage = suggestion
			}
		case journal.EventRunResumed:
			// A resume reopens a terminal run. Clearing finished_at matters:
			// leaving it would make a live run look finished to every list.
			row.Phase = journal.PhaseRunning
			row.FinishedAt = nil
			row.CurrentStage = event.Target
			row.OutcomeVerdict, row.OutcomeTarget = "", ""
		case journal.EventGateOverridden:
			row.Phase = journal.PhaseRunning
			row.FinishedAt = nil
			row.CurrentStage = event.Target
			row.OutcomeVerdict = event.Verdict
			row.OutcomeTarget = event.Target
		case journal.EventStageStarted:
			row.CurrentStage = event.Stage
			s := stageRow(stages, row.RunID, event.Stage)
			s.Attempts++
			s.openAttempts++
			if s.StartedAt == nil {
				at := event.Time
				s.StartedAt = &at
			}
			nodeRow(nodes, row.RunID, "stage", event.Stage, "").
				recordAttempt(event.Branch, event.AttemptClass)

		case journal.EventError:
			if event.Stage == "" || event.Error == nil || event.Error.Code != "executor_error" {
				continue
			}
			if row.CurrentStage == event.Stage {
				row.CurrentStage = ""
			}
			s := stageRow(stages, row.RunID, event.Stage)
			if s.openAttempts > 0 {
				s.openAttempts--
			} else {
				// The journal reference synthesizes an attempt when dispatch
				// fails before stage.started can be recorded.
				s.Attempts++
				nodeRow(nodes, row.RunID, "stage", event.Stage, "").
					recordAttempt(event.Branch, event.AttemptClass)
			}
			s.LastStatus = "failure"
			s.LastAttemptClass = string(event.AttemptClass)
			s.HadFailure = true
			at := event.Time
			s.FinishedAt = &at
		case journal.EventStageFinished:
			if row.CurrentStage == event.Stage {
				row.CurrentStage = ""
			}
			s := stageRow(stages, row.RunID, event.Stage)
			if s.openAttempts > 0 {
				s.openAttempts--
			}
			s.LastStatus = event.Status
			s.LastAttemptClass = string(event.AttemptClass)
			switch event.Status {
			case "success":
				s.HadSuccess = true
			case "failure":
				s.HadFailure = true
			default:
				s.HadOther = true
			}
			at := event.Time
			s.FinishedAt = &at
			if row.Operator.IssueTitle == "" {
				id, idOK := event.Outputs["id"].(string)
				title, titleOK := event.Outputs["title"].(string)
				if idOK && titleOK && id != "" && title != "" {
					row.Operator.IssueNumber = id
					row.Operator.IssueTitle = title
				}
			}
			countAttempt(&row, event)
		case journal.EventGateStarted:
			row.CurrentStage = event.Gate
			node := nodeRow(nodes, row.RunID, "gate", event.Gate, "")
			node.recordAttempt(event.Branch, "")
			node.gateOpen[event.Branch] = true
		case journal.EventGateEvaluated:
			if row.CurrentStage == event.Gate {
				row.CurrentStage = ""
			}
			node := nodeRow(nodes, row.RunID, "gate", event.Gate, "")
			if !node.gateOpen[event.Branch] {
				node.recordAttempt(event.Branch, "")
			}
			node.gateOpen[event.Branch] = false
			row.OutcomeVerdict = event.Verdict
			row.OutcomeTarget = event.Target
			if event.Gate == "review" {
				row.Operator.ReviewVerdict = event.Verdict
				row.Operator.ReviewRationale = ""
				row.Operator.ReviewProblem = ""
			}
		case journal.EventRefTouched:
			if event.ExternalRef == nil {
				continue
			}
			switch event.ExternalRef.Kind {
			case "issue":
				if row.Operator.IssueTitle == "" {
					row.Operator.IssueNumber = event.ExternalRef.ID
				}
				operation, _ := event.Runner["operation"].(string)
				row.Operator.ProviderClaimRecorded =
					row.Operator.ProviderClaimRecorded || operation == "claim"
			case "pr":
				ref := *event.ExternalRef
				row.Operator.PullRequest = &ref
			}
		case journal.EventStageRerunRequested:
			// A repass reopens the run for the same reason a resume does.
			row.Phase = journal.PhaseRunning
			row.FinishedAt = nil
			row.CurrentStage = event.Stage
			row.RepassCount++
			nodeRow(nodes, row.RunID, "stage", event.Stage, "").
				humanRequested[event.Branch] = true
		case journal.EventRunFinished:
			// The journal-derived reference closes attempts still open at run
			// termination as failures. Mirror that before projecting the run's
			// terminal state so outcome=failure sees the same attempt set.
			for _, stage := range stages {
				if stage.openAttempts > 0 {
					stage.HadFailure = true
					stage.openAttempts = 0
				}
			}
			phase := journal.RunPhase(event.Status)
			// An unrecognised terminal status is NOT silently accepted: writing
			// it would make phase — an indexed, filtered column — carry a value
			// no query predicate expects, and the run would quietly vanish from
			// every phase-filtered list. Leave the run as it was; repair and the
			// differential oracle surface the discrepancy.
			if !terminalPhase(phase) {
				continue
			}
			row.Phase = phase
			finished := event.Time
			row.FinishedAt = &finished
			if !strings.HasPrefix(row.CurrentStage, workspaceResetSuggestionPrefix) {
				row.CurrentStage = ""
			}
		}
	}

	row.Terminal = row.Phase != journal.PhaseRunning
	row.Stages = sortedStages(seenStages)

	out := make([]StageRow, 0, len(stages))
	for _, s := range stages {
		out = append(out, *s)
	}
	// Sorted so a rebuild produces byte-identical rows in a stable order
	// (§14.9); map iteration order would defeat that.
	sort.Slice(out, func(i, j int) bool { return out[i].Stage < out[j].Stage })
	outNodes := make([]NodeRow, 0, len(nodes))
	for _, node := range nodes {
		outNodes = append(outNodes, *node)
	}
	sort.Slice(outNodes, func(i, j int) bool {
		if outNodes[i].Kind != outNodes[j].Kind {
			return outNodes[i].Kind < outNodes[j].Kind
		}
		if outNodes[i].Name != outNodes[j].Name {
			return outNodes[i].Name < outNodes[j].Name
		}
		return outNodes[i].Identity < outNodes[j].Identity
	})

	// Recomputed from the full fold every time (not carried from prev), so
	// incremental and whole-history projection agree (§14.9) exactly like
	// row.Stages above.
	row.Disposition = DispositionUnknown
	if row.Phase == journal.PhaseCompleted && len(out) == 1 && out[0].LastStatus == DispositionNoWork {
		row.Disposition = DispositionNoWork
	}

	return Projection{Run: row, Stages: out, Nodes: outNodes}
}

// ProjectRunFromJournal adds facts that require resolving immutable journal
// blobs to the otherwise pure event projection.
func ProjectRunFromJournal(reader *journal.Reader, identity journal.RunIdentity, events []journal.Event) (Projection, error) {
	projection := ProjectRun(identity, Projection{}, events)
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if !event.KnownSchema() || event.Type != journal.EventGateEvaluated ||
			event.Gate != "review" {
			continue
		}
		if event.Ref == nil {
			break
		}
		data, err := reader.ArtifactBytes(*event.Ref)
		if err != nil {
			projection.Run.Operator.ReviewProblem = fmt.Sprintf("review rationale unavailable: %v", err)
			break
		}
		var verdict struct {
			Rationale string `json:"rationale"`
			Summary   string `json:"summary"`
		}
		if err := json.Unmarshal(data, &verdict); err != nil {
			projection.Run.Operator.ReviewProblem = fmt.Sprintf("review rationale is invalid: %v", err)
			break
		}
		projection.Run.Operator.ReviewRationale = strings.TrimSpace(verdict.Rationale)
		if projection.Run.Operator.ReviewRationale == "" {
			projection.Run.Operator.ReviewRationale = strings.TrimSpace(verdict.Summary)
		}
		break
	}

	for _, input := range identity.Inputs {
		if input.Name != journal.PinnedWorkflowGraphInputName {
			continue
		}
		data, err := reader.ArtifactBytes(input.Ref)
		if err != nil {
			return Projection{}, fmt.Errorf("readmodel: read pinned graph: %w", err)
		}
		var graph workflow.Graph
		if err := json.Unmarshal(data, &graph); err != nil {
			return Projection{}, fmt.Errorf("readmodel: parse pinned graph: %w", err)
		}
		if graph.Name != identity.Workflow || graph.Version != identity.WorkflowVersion ||
			graph.Digest != identity.WorkflowDigest {
			return Projection{}, fmt.Errorf("readmodel: pinned graph identity does not match run")
		}
		for _, node := range graph.Nodes {
			if operatorTrajectory(node.ID, journal.PhaseRunning) == "open PR" {
				projection.Run.Operator.PROpenerStage = node.ID
				break
			}
		}
		break
	}
	if projection.Run.Operator.PullRequest != nil {
		projection.Run.Operator.PROpenerStage = ""
	}
	return projection, nil
}

func operatorTrajectory(stage string, phase journal.RunPhase) string {
	if phase != journal.PhaseRunning {
		return "parked"
	}
	stage = strings.ToLower(stage)
	switch {
	case strings.Contains(stage, "review"):
		return "review"
	case strings.Contains(stage, "local-ci"), strings.Contains(stage, "local_ci"):
		return "local CI"
	case strings.Contains(stage, "push"):
		return "push"
	case strings.Contains(stage, "open-pr"), strings.Contains(stage, "open_pr"):
		return "open PR"
	case strings.Contains(stage, "poll"), strings.Contains(stage, "ci-status"):
		return "CI poll"
	case strings.Contains(stage, "close-out"), strings.Contains(stage, "close_out"):
		return "close-out"
	case strings.Contains(stage, "implement"):
		return "implementing"
	default:
		return "parked"
	}
}

const workspaceResetSuggestionPrefix = "Workspace reset suggested:"

// RunnerResetSuggestion projects pinned-workspace recovery guidance into the
// run summary field rendered by portal run lists.
func RunnerResetSuggestion(event journal.Event) (string, bool) {
	if event.Type != journal.EventRunnerAnnotation ||
		event.Runner["kind"] != "workspace_reset_suggested" ||
		event.Runner["workspaceMode"] != "pinned" {
		return "", false
	}
	suggestion, ok := event.Runner["suggestion"].(string)
	if !ok || suggestion == "" {
		return "", false
	}
	return workspaceResetSuggestionPrefix + " " + suggestion, true
}

// RunnerQueueStatus projects pinned-workspace lease bookkeeping into the
// operator-visible current-stage slot without changing the normative journal.
func RunnerQueueStatus(event journal.Event) (string, bool) {
	if event.Type != journal.EventRunnerAnnotation || event.Runner["workspaceMode"] != "pinned" {
		return "", false
	}
	var position int
	switch value := event.Runner["queuePosition"].(type) {
	case int:
		position = value
	case float64:
		position = int(value)
	default:
		return "", false
	}
	if position <= 0 {
		return "", true
	}
	return fmt.Sprintf("Workspace queue (position %d)", position), true
}

// countAttempt folds a finished stage attempt into the run's retry counters.
//
// The three classes are distinct on purpose (#849): a policy retry is the
// workflow deciding to try again, an infra retry is the machinery failing, and
// the total matters for neither. Collapsing them would make "is this workflow
// flaky or is the runner" unanswerable.
func countAttempt(row *RunRow, event journal.Event) {
	switch event.AttemptClass {
	case journal.AttemptPolicy:
		row.PolicyRetryCount++
		row.RetryCount++
	case journal.AttemptInfra:
		row.InfraRetryCount++
		row.RetryCount++
	}
}

// carryStages rebuilds the mutable stage accumulators from a previous
// projection, so an incremental pass continues counting rather than restarting.
func carryStages(prev []StageRow) map[string]*StageRow {
	out := make(map[string]*StageRow, len(prev))
	for i := range prev {
		row := prev[i]
		out[row.Stage] = &row
	}
	return out
}

func carryNodes(prev []NodeRow) map[string]*NodeRow {
	out := make(map[string]*NodeRow, len(prev))
	for i := range prev {
		row := prev[i]
		row.branchAttempts = cloneIntMap(row.branchAttempts)
		row.gateOpen = cloneBoolMap(row.gateOpen)
		row.humanVisit = cloneBoolMap(row.humanVisit)
		row.humanRequested = cloneBoolMap(row.humanRequested)
		out[nodeKey(row.Kind, row.Name, row.Identity)] = &row
	}
	return out
}

func nodeRow(nodes map[string]*NodeRow, runID, kind, name, identity string) *NodeRow {
	key := nodeKey(kind, name, identity)
	if node, ok := nodes[key]; ok {
		return node
	}
	node := &NodeRow{
		RunID: runID, Kind: kind, Name: name, Identity: identity,
		branchAttempts: make(map[int]int), gateOpen: make(map[int]bool),
		humanVisit: make(map[int]bool), humanRequested: make(map[int]bool),
	}
	nodes[key] = node
	return node
}

func (n *NodeRow) recordAttempt(branch int, class journal.AttemptClass) {
	if n.branchAttempts == nil {
		n.branchAttempts = make(map[int]int)
	}
	if n.gateOpen == nil {
		n.gateOpen = make(map[int]bool)
	}
	if n.humanVisit == nil {
		n.humanVisit = make(map[int]bool)
	}
	if n.humanRequested == nil {
		n.humanRequested = make(map[int]bool)
	}
	newTraversal := class == ""
	switch class {
	case journal.AttemptHuman:
		newTraversal = n.humanRequested[branch] || !n.humanVisit[branch]
		n.humanVisit[branch] = true
		n.humanRequested[branch] = false
	case "":
		n.humanVisit[branch] = false
		n.humanRequested[branch] = false
	}
	if newTraversal && n.branchAttempts[branch] > 0 {
		n.RetryWasteAttempts += n.branchAttempts[branch]
		n.branchAttempts[branch] = 0
	}
	n.Attempts++
	n.branchAttempts[branch]++
}

func nodeKey(kind, name, identity string) string {
	return kind + "\x00" + name + "\x00" + identity
}

func cloneIntMap(in map[int]int) map[int]int {
	out := make(map[int]int, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneBoolMap(in map[int]bool) map[int]bool {
	out := make(map[int]bool, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// stageRow fetches or creates the accumulator for a stage.
func stageRow(stages map[string]*StageRow, runID, stage string) *StageRow {
	if s, ok := stages[stage]; ok {
		return s
	}
	s := &StageRow{RunID: runID, Stage: stage}
	stages[stage] = s
	return s
}

// stageSet rebuilds the seen-stage set from a previously projected row, so an
// incremental projection does not forget stages it has already recorded.
func stageSet(stages []string) map[string]struct{} {
	out := make(map[string]struct{}, len(stages))
	for _, s := range stages {
		out[s] = struct{}{}
	}
	return out
}

// sortedStages returns the stage set in a stable order.
func sortedStages(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// terminalPhase reports whether phase is a recognised terminal run phase.
//
// PhaseRunning is excluded deliberately: a run.finished carrying "running" is
// contradictory, and accepting it would leave a row marked terminal while its
// phase says otherwise.
func terminalPhase(phase journal.RunPhase) bool {
	switch phase {
	case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseEscalated, journal.PhaseAborted:
		return true
	default:
		return false
	}
}
