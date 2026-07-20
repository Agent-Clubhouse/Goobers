package localscheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

const triggerEvaluationsFileName = "trigger-evaluations.json"

type triggerEvaluationsFile struct {
	Workflows []triggerEvaluation `json:"workflows"`
}

type triggerEvaluation struct {
	Gaggle   string    `json:"gaggle"`
	Workflow string    `json:"workflow"`
	LastEval time.Time `json:"lastEval"`
}

// ReadTriggerEvaluations returns the scheduler's latest live LastEval value
// for each workflow. A missing file means no scheduler has reconciled this
// instance yet.
func ReadTriggerEvaluations(schedulerDir string) (map[WorkflowIdentity]time.Time, error) {
	path := filepath.Join(schedulerDir, triggerEvaluationsFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[WorkflowIdentity]time.Time{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("localscheduler: read trigger evaluations: %w", err)
	}

	var state triggerEvaluationsFile
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("localscheduler: decode trigger evaluations: %w", err)
	}
	evaluations := make(map[WorkflowIdentity]time.Time, len(state.Workflows))
	for _, workflow := range state.Workflows {
		identity := WorkflowIdentity{Gaggle: workflow.Gaggle, Workflow: workflow.Workflow}
		if identity.Workflow == "" || workflow.LastEval.IsZero() {
			return nil, fmt.Errorf("localscheduler: invalid trigger evaluation for workflow %q", identity.Workflow)
		}
		if _, exists := evaluations[identity]; exists {
			return nil, fmt.Errorf("localscheduler: duplicate trigger evaluation for workflow %q in gaggle %q", identity.Workflow, identity.Gaggle)
		}
		evaluations[identity] = workflow.LastEval
	}
	return evaluations, nil
}

func writeTriggerEvaluations(schedulerDir string, evaluations map[WorkflowIdentity]time.Time) error {
	state := triggerEvaluationsFile{
		Workflows: make([]triggerEvaluation, 0, len(evaluations)),
	}
	for identity, lastEval := range evaluations {
		state.Workflows = append(state.Workflows, triggerEvaluation{
			Gaggle:   identity.Gaggle,
			Workflow: identity.Workflow,
			LastEval: lastEval,
		})
	}
	sort.Slice(state.Workflows, func(i, j int) bool {
		if state.Workflows[i].Gaggle == state.Workflows[j].Gaggle {
			return state.Workflows[i].Workflow < state.Workflows[j].Workflow
		}
		return state.Workflows[i].Gaggle < state.Workflows[j].Gaggle
	})
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("localscheduler: marshal trigger evaluations: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		return fmt.Errorf("localscheduler: create trigger evaluation directory: %w", err)
	}
	if err := journal.WriteFileAtomic(filepath.Join(schedulerDir, triggerEvaluationsFileName), data, 0o644); err != nil {
		return fmt.Errorf("localscheduler: persist trigger evaluations: %w", err)
	}
	return nil
}
