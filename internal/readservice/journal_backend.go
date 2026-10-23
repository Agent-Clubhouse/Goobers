package readservice

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

func (s *Local) instanceJournalUpdatedAt(ctx context.Context) (time.Time, error) {
	if s.sources.Journal == nil {
		path, err := journal.InstanceEventsPath(s.sources.Layout.SchedulerDir())
		if err != nil {
			return time.Time{}, fmt.Errorf("resolve instance journal path: %w", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return time.Time{}, fmt.Errorf("read instance journal freshness: %w", err)
		}
		return info.ModTime().UTC(), nil
	}
	// Pages keep memory bounded; freshness comes from committed event times,
	// never a local file belonging to a different backend.
	var updated time.Time
	var after uint64
	for {
		events, err := s.journalReader().InstanceRead(ctx, after, 256)
		if err != nil {
			return time.Time{}, fmt.Errorf("read instance journal freshness: %w", err)
		}
		for _, event := range events {
			if event.Time.After(updated) {
				updated = event.Time
			}
		}
		if len(events) == 0 {
			return updated.UTC(), nil
		}
		next := events[len(events)-1].Seq
		if next <= after {
			return time.Time{}, fmt.Errorf("instance journal cursor did not advance")
		}
		after = next
	}
}

func (s *Local) backendActiveRunCounts(ctx context.Context) (map[localscheduler.WorkflowIdentity]int, error) {
	counts := make(map[localscheduler.WorkflowIdentity]int)
	var after string
	for {
		refs, err := s.journalReader().ListRuns(ctx, journal.RunFilter{After: after, Limit: 256})
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			reader, err := s.journalReader().OpenRun(ctx, ref)
			if err != nil {
				return nil, err
			}
			identity, err := reader.Identity()
			if err != nil {
				return nil, err
			}
			phase, err := reader.PhaseBounded(ctx)
			if err != nil {
				return nil, err
			}
			if phase == journal.PhaseRunning {
				if identity.Child == nil {
					events, err := reader.Events()
					if err != nil {
						return nil, err
					}
					if journal.ParkedOnChild(events) {
						continue
					}
				}
				counts[localscheduler.WorkflowIdentity{Gaggle: identity.Gaggle, Workflow: identity.Workflow}]++
			}
		}
		if len(refs) == 0 {
			return counts, nil
		}
		next := refs[len(refs)-1].ID
		if next <= after {
			return nil, fmt.Errorf("journal run cursor did not advance")
		}
		after = next
	}
}
