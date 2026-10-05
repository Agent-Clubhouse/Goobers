package restartintent

import (
	"context"
	"encoding/json"
)

// Dependencies protects exact source journals and configuration until the epoch
// owns its snapshot. Corruption blocks pruning rather than dropping custody.
type Dependencies struct {
	Generations map[string]bool
	Runs        map[string]map[string]bool
}

// Retained inventories only starts not yet proven durably published.
func (s *Service) Retained(ctx context.Context) (Dependencies, error) {
	d := Dependencies{Generations: map[string]bool{}, Runs: map[string]map[string]bool{}}
	var after string
	for {
		page, err := s.Queue.RetainedPage(ctx, after, 100)
		if err != nil {
			return d, err
		}
		for _, r := range page {
			after = r.ID
			var header struct {
				Kind string `json:"kind"`
			}
			if err = json.Unmarshal(r.Payload, &header); err != nil {
				return d, err
			}
			if header.Kind != Kind {
				continue
			}
			plan, err := s.Load(ctx, r)
			if err != nil {
				return d, err
			}
			d.Generations[plan.Source.ConfigGeneration] = true
			if d.Runs[plan.Source.Gaggle] == nil {
				d.Runs[plan.Source.Gaggle] = map[string]bool{}
			}
			d.Runs[plan.Source.Gaggle][plan.Source.RunID] = true
			d.Runs[plan.Source.Gaggle][plan.Continuation.RunID] = true
			for _, ptr := range plan.Continuation.ContextPointers {
				if ptr.RunID != "" {
					d.Runs[plan.Source.Gaggle][ptr.RunID] = true
				}
			}
		}
		if len(page) == 0 {
			return d, nil
		}
	}
}
