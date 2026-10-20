package readservice

import (
	"context"
	"sort"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/runlineage"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

type continuationReader interface {
	GetRun(context.Context, string) (readmodel.RunRow, bool, error)
	ContinuationRuns(context.Context, string, string) (runlineage.Page, error)
}

type lineageAncestor struct {
	phase    journal.RunPhase
	repasses int
	parent   string
	found    bool
}

func accumulateLineage(ctx context.Context, id string, lineage *RunLineage, lookup func(string) (lineageAncestor, error)) error {
	if lineage == nil || lineage.Source == nil {
		return ctx.Err()
	}
	lineage.HistoricalRepassCount = 0
	lineage.HistoricalRepassCountPartial = false
	seen := map[string]bool{id: true}
	sourceID := lineage.Source.ID
	for depth := 0; sourceID != ""; depth++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth == runlineage.MaxDepth || seen[sourceID] {
			lineage.HistoricalRepassCountPartial = true
			break
		}
		seen[sourceID] = true
		source, err := lookup(sourceID)
		if err != nil {
			return err
		}
		if !source.found {
			lineage.HistoricalRepassCountPartial = true
			break
		}
		if depth == 0 {
			lineage.Source.Phase = source.phase
		}
		lineage.HistoricalRepassCount += source.repasses
		sourceID = source.parent
	}
	return nil
}

func lineagePage(lineage *RunLineage, page runlineage.Page) *RunLineage {
	if lineage == nil && len(page.Runs) == 0 && !page.Incomplete {
		return nil
	}
	if lineage == nil {
		lineage = &RunLineage{}
	}
	lineage.Continuations = nil
	for _, run := range page.Runs {
		lineage.Continuations = append(lineage.Continuations, LineageRun{ID: run.ID, Phase: journal.RunPhase(run.Phase)})
	}
	lineage.ContinuationsNextCursor = page.NextCursor
	lineage.ContinuationsTruncated = page.NextCursor != ""
	lineage.ContinuationsIncomplete = page.Incomplete
	return lineage
}

func (s *Local) offlineRunLineagePage(ctx context.Context, target RunSummary, after string) (*RunLineage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pages, err := withTelemetryDB(s, []runlineage.Page{}, func(db *rollup.DB) ([]runlineage.Page, error) {
		page, err := db.ContinuationRuns(ctx, target.ID, after)
		return []runlineage.Page{page}, err
	})
	if err != nil {
		return nil, err
	}
	page := runlineage.Page{Incomplete: true}
	if len(pages) > 0 {
		page = pages[0]
	}
	lineage := lineagePage(target.Lineage, page)
	err = accumulateLineage(ctx, target.ID, lineage, func(id string) (lineageAncestor, error) {
		run, err := s.openRun(id)
		if err != nil {
			return lineageAncestor{}, nil
		}
		summary, err := summarizeRun(run, s.now().UTC())
		if err != nil {
			return lineageAncestor{}, nil
		}
		parent := ""
		if summary.Lineage != nil && summary.Lineage.Source != nil {
			parent = summary.Lineage.Source.ID
		}
		return lineageAncestor{phase: summary.Phase, repasses: summary.RepassCount, parent: parent, found: true}, nil
	})
	return lineage, err
}

func (s *Local) decorateRunLineage(ctx context.Context, runs []RunSummary, cursor ...string) error {
	projected, ok := s.sources.ReadModel.(continuationReader)
	if !ok || !s.readModelReads {
		return nil
	}
	after := ""
	if len(cursor) > 0 {
		after = cursor[0]
	}
	// Shared ancestors on a list page require at most one indexed lookup each.
	cache := make(map[string]lineageAncestor)
	lookup := func(id string) (lineageAncestor, error) {
		if cached, ok := cache[id]; ok {
			return cached, nil
		}
		row, found, err := projected.GetRun(ctx, id)
		if err != nil {
			return lineageAncestor{}, err
		}
		ancestor := lineageAncestor{phase: row.Phase, repasses: row.RepassCount, parent: row.Operator.ContinuedFromRunID, found: found}
		cache[id] = ancestor
		return ancestor, nil
	}
	for i := range runs {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := projected.ContinuationRuns(ctx, runs[i].ID, after)
		if err != nil {
			return err
		}
		runs[i].Lineage = lineagePage(runs[i].Lineage, page)
		if err := accumulateLineage(ctx, runs[i].ID, runs[i].Lineage, lookup); err != nil {
			return err
		}
	}
	return nil
}

func decorateRunLineageFromSummaries(ctx context.Context, runs []RunSummary) error {
	byID := make(map[string]*RunSummary, len(runs))
	for i := range runs {
		byID[runs[i].ID] = &runs[i]
	}
	for i := range runs {
		if err := ctx.Err(); err != nil {
			return err
		}
		lineage := runs[i].Lineage
		if lineage == nil || lineage.Source == nil {
			continue
		}
		if source := byID[lineage.Source.ID]; source != nil {
			if source.Lineage == nil {
				source.Lineage = &RunLineage{}
			}
			children := append(source.Lineage.Continuations, LineageRun{ID: runs[i].ID, Phase: runs[i].Phase})
			sort.Slice(children, func(i, j int) bool { return children[i].ID < children[j].ID })
			if len(children) > runlineage.PageSize {
				children = children[:runlineage.PageSize]
				source.Lineage.ContinuationsTruncated = true
			}
			source.Lineage.Continuations = children
			if source.Lineage.ContinuationsTruncated {
				source.Lineage.ContinuationsNextCursor = children[len(children)-1].ID
			}
		}
		if err := accumulateLineage(ctx, runs[i].ID, lineage, func(id string) (lineageAncestor, error) {
			source := byID[id]
			if source == nil {
				return lineageAncestor{}, nil
			}
			parent := ""
			if source.Lineage != nil && source.Lineage.Source != nil {
				parent = source.Lineage.Source.ID
			}
			return lineageAncestor{phase: source.Phase, repasses: source.RepassCount, parent: parent, found: true}, nil
		}); err != nil {
			return err
		}
	}
	return nil
}
