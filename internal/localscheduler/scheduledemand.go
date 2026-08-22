package localscheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/goobers/goobers/internal/journal"
)

const scheduleDemandStateFileName = "schedule-demand.json"

type scheduleDemandStateFile struct {
	// Owner/Generation are the M5 ownership stamp (stateguard.go). Readers
	// ignore them; the writing scheduler's stateOwner checks them so a second
	// daemon trips ErrStateSeized instead of silently interleaving writes.
	Owner      string                   `json:"owner,omitempty"`
	Generation int64                    `json:"generation,omitempty"`
	Workflows  []scheduleDemandWorkflow `json:"workflows"`
}

type scheduleDemandWorkflow struct {
	Gaggle   string `json:"gaggle"`
	Workflow string `json:"workflow"`
}

func readScheduleDemandState(schedulerDir string) (map[WorkflowIdentity]bool, error) {
	data, err := os.ReadFile(filepath.Join(schedulerDir, scheduleDemandStateFileName))
	if errors.Is(err, os.ErrNotExist) {
		return map[WorkflowIdentity]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("localscheduler: read schedule demand: %w", err)
	}
	var state scheduleDemandStateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("localscheduler: decode schedule demand: %w", err)
	}
	outstanding := make(map[WorkflowIdentity]bool, len(state.Workflows))
	for _, workflow := range state.Workflows {
		identity := WorkflowIdentity(workflow)
		if identity.Workflow == "" {
			return nil, fmt.Errorf("localscheduler: invalid schedule demand for empty workflow")
		}
		if outstanding[identity] {
			return nil, fmt.Errorf(
				"localscheduler: duplicate schedule demand for workflow %q in gaggle %q",
				identity.Workflow, identity.Gaggle,
			)
		}
		outstanding[identity] = true
	}
	return outstanding, nil
}

func writeScheduleDemandState(schedulerDir string, owner *stateOwner, outstanding map[WorkflowIdentity]bool) error {
	stamp, err := owner.stamp(schedulerDir, scheduleDemandStateFileName)
	if err != nil {
		return err
	}
	state := scheduleDemandStateFile{
		Owner:      stamp.Owner,
		Generation: stamp.Generation,
		Workflows:  make([]scheduleDemandWorkflow, 0, len(outstanding)),
	}
	for identity, pending := range outstanding {
		if !pending {
			continue
		}
		state.Workflows = append(state.Workflows, scheduleDemandWorkflow(identity))
	}
	sort.Slice(state.Workflows, func(i, j int) bool {
		if state.Workflows[i].Gaggle == state.Workflows[j].Gaggle {
			return state.Workflows[i].Workflow < state.Workflows[j].Workflow
		}
		return state.Workflows[i].Gaggle < state.Workflows[j].Gaggle
	})
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("localscheduler: marshal schedule demand: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		return fmt.Errorf("localscheduler: create schedule demand directory: %w", err)
	}
	if err := journal.WriteFileAtomic(filepath.Join(schedulerDir, scheduleDemandStateFileName), data, 0o644); err != nil {
		return fmt.Errorf("localscheduler: persist schedule demand: %w", err)
	}
	// Only a landed write commits the claimed generation: a failed write must
	// stay retryable rather than poisoning later writes with ErrStateSeized.
	owner.commit(scheduleDemandStateFileName, stamp)
	return nil
}
