package runner

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
)

// runAutomated binds the concrete artifact-aware evaluator to this walk's
// already-open journal identity. Copying the evaluator keeps concurrent runs
// from rebinding each other's readers; custom scalar evaluator seams are intact.
func (r *Runner) runAutomated(in StartInput, jr executionJournal) invoke.Automated {
	automated, ok := r.cfg.Automated.(*gate.AutomatedEvaluator)
	if !ok || automated == nil {
		return r.cfg.Automated
	}
	bound := *automated
	bound.OpenArtifacts = func(ctx context.Context, runID, gaggle string) (gate.ArtifactReader, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if runID != in.RunID || gaggle != in.Gaggle {
			return nil, fmt.Errorf("%w: artifact reader belongs to another run", artifactset.ErrInvalid)
		}
		return artifactset.OpenJournal(jr.Dir())
	}
	return &bound
}
