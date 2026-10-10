package parallelworkspace

import (
	"encoding/json"
	"errors"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
)

type joinEvidence struct {
	planned  bool
	results  map[int]bool
	finished map[int]bool
}

func validateJoinEvidence(reader *journal.Reader, request spec.JoinRequest) error {
	events, err := reader.Events()
	if err != nil {
		return err
	}
	evidence := joinEvidence{results: map[int]bool{}, finished: map[int]bool{}}
	for _, event := range events {
		if event.Seq <= request.Sequence {
			continue
		}
		if event.Type == journal.EventParallelStarted && event.Branch == 0 {
			break
		}
		if event.Parallel != request.Parallel {
			continue
		}
		if err := evidence.consume(event, request); err != nil {
			return err
		}
	}
	if !evidence.planned || len(evidence.finished) != len(request.Results) {
		return errors.New("parallel join lacks recorded plan and stopped branch receipts")
	}
	return nil
}

func (s *joinEvidence) consume(event journal.Event, request spec.JoinRequest) error {
	if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == "isolated.parent.fork.planned" {
		var ref journal.Ref
		data, err := json.Marshal(event.Runner["plan"])
		if err != nil || json.Unmarshal(data, &ref) != nil || ref != request.Plan || event.Branch != 0 || s.planned {
			return errors.New("parallel join plan is not its durable root reservation")
		}
		s.planned = true
	}
	if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == "isolated.parent.fork.result" {
		return s.result(event, request)
	}
	if event.Type == journal.EventBranchFinished {
		index := event.Branch - 1
		if index < 0 || index >= len(request.Results) || !s.results[event.Branch] || s.finished[event.Branch] || event.BranchStatus != request.Results[index].Status {
			return errors.New("parallel join branch was not settled after its exact immutable result")
		}
		s.finished[event.Branch] = true
	}
	return nil
}

func (s *joinEvidence) result(event journal.Event, request spec.JoinRequest) error {
	var result struct {
		Sequence uint64
		Plan     journal.Ref
		Branch   int
		Status   journal.BranchStatus
		Source   spec.Source
	}
	data, err := json.Marshal(event.Runner["result"])
	if err != nil || len(data) > 8192 || json.Unmarshal(data, &result) != nil {
		return errors.New("invalid parallel join result receipt")
	}
	index := result.Branch - 1
	if !s.planned || result.Sequence != request.Sequence || result.Plan != request.Plan || result.Branch != event.Branch || index < 0 || index >= len(request.Results) || s.results[result.Branch] {
		return errors.New("parallel join result changed reservation or branch")
	}
	expected := request.Results[index]
	if result.Status != expected.Status || result.Source != expected.Source {
		return errors.New("parallel join substituted a recorded branch result")
	}
	s.results[result.Branch] = true
	return nil
}
