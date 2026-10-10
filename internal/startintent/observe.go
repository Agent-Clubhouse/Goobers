package startintent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// VerifyIdentity compares published execution to the exact accepted pins.
func VerifyIdentity(id journal.RunIdentity, record triggerqueue.Record) error {
	e, err := Parse(record.Payload)
	if err != nil {
		return err
	}
	t := e.Target
	if id.RunID != strings.TrimPrefix(record.ID, "trigger-") || id.Gaggle != t.Gaggle || id.Workflow != t.Workflow || id.ConfigGeneration != t.ConfigGeneration || id.WorkflowDigest != t.WorkflowDigest || id.GooberDigest != t.GooberDigest || id.Child != nil || id.ContinuedFromRunID != "" {
		return errors.New("startintent: published execution differs from acceptance")
	}
	want := journal.Trigger{Kind: journal.TriggerManual, Ref: t.Workflow}
	if e.Request.SourceRun != "" {
		want = journal.Trigger{Kind: journal.TriggerSignal, Ref: "priority-re-tick:" + e.Request.SourceRun}
	}

	if e.Source != nil {
		want = e.Source.Trigger(t.Workflow)
	}
	if id.Trigger != want {
		return errors.New("startintent: published trigger differs from acceptance")
	}
	return nil
}

// Observe accepts only a durable matching journal.
func (s *Service) Observe(ctx context.Context, record triggerqueue.Record) (bool, error) {
	runID := strings.TrimPrefix(record.ID, "trigger-")
	if record.RunID != "" && record.RunID != runID {
		return false, triggerqueue.ErrConflict
	}
	if s.RunDirectory == nil {
		return false, errors.New("startintent: journal observer unavailable")
	}
	dir, err := s.RunDirectory(ctx, runID)
	if err != nil || dir == "" {
		return false, err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return false, err
	}
	id, err := reader.Identity()
	if err != nil {
		return false, err
	}
	if err = VerifyIdentity(id, record); err != nil {
		return false, err
	}
	return true, nil
}

// RetainedGenerations fails closed if any accepted typed envelope is corrupt.
// Already dispatched runs use the ordinary retained-journal generation owner.
func RetainedGenerations(ctx context.Context, queue *triggerqueue.Store) (map[string]bool, error) {
	pins, err := retainedScheduleDemandGenerations(ctx, queue)
	if err != nil {
		return nil, err
	}
	var after string
	for {
		page, err := queue.RetainedPage(ctx, after, 100)
		if err != nil {
			return nil, err
		}
		for _, record := range page {
			after = record.ID
			var header struct {
				Kind string `json:"kind"`
			}
			if err = json.Unmarshal(record.Payload, &header); err != nil {
				return nil, err
			}
			if header.Kind != Kind {
				continue
			}
			e, err := Parse(record.Payload)
			if err != nil {
				return nil, err
			}
			pins[e.Target.ConfigGeneration] = true
		}
		if len(page) < 100 {
			return pins, nil
		}
	}
}
