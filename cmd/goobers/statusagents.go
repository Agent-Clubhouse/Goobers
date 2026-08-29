package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// The self-excluding process probe (#3362).
//
// Every operator of a goobers instance eventually needs to answer "is agentic
// work live right now?" — before a deploy, during a debug, when sizing a quiet
// window. Without a first-class answer they write the probe themselves, and
// the probe they write is a process-table grep:
//
//	ps aux | grep copilot | grep -v grep
//
// That probe has a famous defect: the grep matches its own shell, so it
// reports busy when the instance is idle. The bracket idiom (`[c]opilot`)
// dodges it, and remembering the bracket idiom is exactly the kind of reflex a
// written rule does not reliably install — an operations night produced two
// operators writing the same probe within an hour, one of whom watched their
// deploy watcher report busy=1 (its own shell) for 13 minutes.
//
// This probe answers the same question from the runner's own bookkeeping —
// the run journals the runner already writes — and never from a process table.
// The asker cannot appear in the answer because the answer is not built from
// anything the asker is in: buildAgentProbe's only inputs are journal-derived
// run summaries and the compiled workflow definitions. On top of that
// structural property it also drops the invoking run itself when this process
// IS a stage (GOOBERS_RUN_ID set), so a stage that probes for live siblings
// never counts itself either — and says so, rather than silently hiding it.
//
// statusagents_guard_test.go pins the structural property so a later change
// cannot quietly reintroduce process matching here.

const (
	// agentProbeSourceJournal names the only sanctioned source of truth for
	// this probe. It is emitted in both the text and JSON renderings so a
	// reader (or a script's author) can see the answer did not come from a
	// process table.
	agentProbeSourceJournal = "runner-journal"

	// agentStageKindTask and agentStageKindGate distinguish an agentic task
	// (a goober doing work) from an agentic gate (a reviewer goober returning
	// a verdict). Both are live agentic work; which one it is changes what an
	// operator should expect it to be doing.
	agentStageKindTask = "task"
	agentStageKindGate = "gate"
	// agentStageKindUnknown marks a live run whose workflow definition is no
	// longer loadable (renamed or removed from config while the run is in
	// flight). It is reported rather than dropped: for a quiet-window
	// decision, "something is live and I cannot classify it" must never
	// render as silence.
	agentStageKindUnknown = "unknown"

	// agentRoleUnknown is the displayed ROLE for an unclassifiable live stage.
	agentRoleUnknown = "unknown"

	agentProbeRowFormat = "%-18.18s  %-34.34s  %-22.22s  %-26.26s  %-10.10s  %s"
)

// liveAgentStage is one in-flight agentic stage: which ROLE is working, in
// which run, at which stage.
type liveAgentStage struct {
	Role            string     `json:"role"`
	RunID           string     `json:"runId"`
	Gaggle          string     `json:"gaggle"`
	Workflow        string     `json:"workflow"`
	Stage           string     `json:"stage"`
	StageKind       string     `json:"stageKind"`
	StartedAt       time.Time  `json:"startedAt"`
	LastActivityAt  time.Time  `json:"lastActivityAt"`
	LastHeartbeatAt *time.Time `json:"lastHeartbeatAt,omitempty"`
	Liveness        string     `json:"liveness"`
}

// selfExcludedRun records the invoking run this probe deliberately dropped.
// It is reported, never silently omitted: an operator must be able to see
// exactly what the self-exclusion removed.
type selfExcludedRun struct {
	RunID    string `json:"runId"`
	Gaggle   string `json:"gaggle,omitempty"`
	Workflow string `json:"workflow,omitempty"`
	Stage    string `json:"stage,omitempty"`
	Role     string `json:"role,omitempty"`
	Reason   string `json:"reason"`
}

// agentProbe is the whole answer to "what agentic work is live right now".
type agentProbe struct {
	// Source is always agentProbeSourceJournal. It is part of the contract:
	// a consumer can assert on it that the answer is bookkeeping-derived.
	Source string `json:"source"`
	// Live is every in-flight agentic stage, ordered by role then run id.
	Live []liveAgentStage `json:"live"`
	// OtherLiveRuns counts running runs that are NOT at an agentic stage —
	// between stages, or in a deterministic one. A quiet window needs this:
	// zero agentic stages with three deterministic runs in flight is not an
	// idle instance.
	OtherLiveRuns int `json:"otherLiveRuns"`
	// SelfExcluded is the invoking run, when this process is itself a stage.
	SelfExcluded *selfExcludedRun `json:"selfExcluded,omitempty"`
}

// stageRole is a workflow stage's agentic classification.
type stageRole struct {
	kind string
	role string
}

// agenticStageRoles indexes every agentic stage in the loaded workflow
// definitions by (gaggle, workflow) then stage name. Parallel branches
// reference tasks and gates declared in these same lists, so branch stages are
// covered without walking Parallels separately.
func agenticStageRoles(workflows []apiv1.Workflow) map[statusWorkflowKey]map[string]stageRole {
	index := make(map[statusWorkflowKey]map[string]stageRole, len(workflows))
	for i := range workflows {
		workflow := &workflows[i]
		key := statusWorkflowKey{gaggle: workflow.Spec.Gaggle, workflow: workflow.Name}
		stages, ok := index[key]
		if !ok {
			stages = make(map[string]stageRole)
			index[key] = stages
		}
		for _, task := range workflow.Spec.Tasks {
			if task.Type != apiv1.TaskAgentic || task.Goober == "" {
				continue
			}
			stages[task.Name] = stageRole{kind: agentStageKindTask, role: task.Goober}
		}
		for _, gate := range workflow.Spec.Gates {
			if gate.Evaluator != apiv1.EvaluatorAgentic || gate.Agentic == nil || gate.Agentic.Goober == "" {
				continue
			}
			stages[gate.Name] = stageRole{kind: agentStageKindGate, role: gate.Agentic.Goober}
		}
	}
	return index
}

// buildAgentProbe projects live agentic work out of journal-derived run
// summaries and the loaded workflow definitions. It is pure: every input is a
// parameter, so it cannot consult a process table, and selfRunID is the
// caller's own run id (empty when this invocation is not a stage).
func buildAgentProbe(
	runs []runSummary,
	workflows []apiv1.Workflow,
	selfRunID, workflowFilter, gaggleFilter string,
) agentProbe {
	index := agenticStageRoles(workflows)
	probe := agentProbe{Source: agentProbeSourceJournal, Live: []liveAgentStage{}}
	for _, run := range runs {
		if run.Phase != journal.PhaseRunning {
			continue
		}
		if workflowFilter != "" && run.Workflow != workflowFilter {
			continue
		}
		if gaggleFilter != "" && run.Gaggle != gaggleFilter {
			continue
		}
		stages, workflowKnown := index[statusWorkflowKey{gaggle: run.Gaggle, workflow: run.Workflow}]
		stage := run.Operator.CurrentStage
		role, agentic := stages[stage]
		if !workflowKnown {
			// Unclassifiable rather than absent — see agentStageKindUnknown.
			role, agentic = stageRole{kind: agentStageKindUnknown, role: agentRoleUnknown}, true
		}
		if selfRunID != "" && run.RunID == selfRunID {
			excluded := &selfExcludedRun{
				RunID:    run.RunID,
				Gaggle:   run.Gaggle,
				Workflow: run.Workflow,
				Stage:    stage,
				Reason:   "this invocation runs as a stage of this run",
			}
			if agentic {
				excluded.Role = role.role
			}
			probe.SelfExcluded = excluded
			continue
		}
		if !agentic {
			probe.OtherLiveRuns++
			continue
		}
		live := liveAgentStage{
			Role:           role.role,
			RunID:          run.RunID,
			Gaggle:         run.Gaggle,
			Workflow:       run.Workflow,
			Stage:          stage,
			StageKind:      role.kind,
			StartedAt:      run.StartedAt,
			LastActivityAt: run.LastActivityAt,
			Liveness:       run.Operator.Liveness,
		}
		if run.Operator.LastHeartbeatAt != nil {
			heartbeat := *run.Operator.LastHeartbeatAt
			live.LastHeartbeatAt = &heartbeat
		}
		probe.Live = append(probe.Live, live)
	}
	sort.Slice(probe.Live, func(i, j int) bool {
		if probe.Live[i].Role == probe.Live[j].Role {
			return probe.Live[i].RunID < probe.Live[j].RunID
		}
		return probe.Live[i].Role < probe.Live[j].Role
	})
	return probe
}

// selfProbeRunID is the invoking run id when this process is itself a stage.
// It is a package var so tests can drive the self-exclusion path without
// mutating process env.
var selfProbeRunID = func() string { return os.Getenv("GOOBERS_RUN_ID") }

func renderAgentProbe(stdout io.Writer, probe agentProbe, now time.Time) {
	pf(stdout, "agentic stages live: %d (source: %s; this invocation can never be a match)\n",
		len(probe.Live), probe.Source)
	if probe.SelfExcluded != nil {
		stage := probe.SelfExcluded.Stage
		if stage == "" {
			stage = "-"
		}
		pf(stdout, "self-excluded: run %s (%s/%s, stage %s) — %s\n",
			probe.SelfExcluded.RunID,
			probe.SelfExcluded.Gaggle,
			probe.SelfExcluded.Workflow,
			stage,
			probe.SelfExcluded.Reason,
		)
	}
	if probe.OtherLiveRuns > 0 {
		pf(stdout, "other live runs not at an agentic stage: %d\n", probe.OtherLiveRuns)
	}
	if len(probe.Live) == 0 {
		pln(stdout, "no agentic stages in flight")
		return
	}
	pf(stdout, agentProbeRowFormat+"\n",
		"ROLE", "RUN ID", "STAGE", "WORKFLOW", "LIVENESS", "LAST ACTIVITY")
	for _, live := range probe.Live {
		stage := live.Stage
		if stage == "" {
			stage = "-"
		}
		// An agentic task is the common case and reads clean unannotated; a
		// gate (reviewer) or an unclassifiable stage is called out, because
		// what an operator should expect the role to be doing differs.
		if live.StageKind != agentStageKindTask {
			stage = stage + " (" + live.StageKind + ")"
		}
		pf(stdout, agentProbeRowFormat+"\n",
			live.Role,
			live.RunID,
			stage,
			live.Gaggle+"/"+live.Workflow,
			live.Liveness,
			formatLastActivity(now, live.LastActivityAt),
		)
	}
}

func emitAgentProbeJSON(stdout io.Writer, probe agentProbe) error {
	if err := json.NewEncoder(stdout).Encode(probe); err != nil {
		return fmt.Errorf("encode agent probe: %w", err)
	}
	return nil
}
