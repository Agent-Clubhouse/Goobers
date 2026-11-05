package cutover

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Step is one idempotent bootstrap action. Issue is non-zero while the
// backing feature has not shipped.
type Step struct {
	Name  string
	Issue int
	// Run reports whether it changed anything.
	Run func(ctx context.Context) (bool, error)
	// Check is a side-effect-free probe of the step's dependency (database
	// reachable, bucket reachable, ...). Bootstrap runs every Check before the
	// first Run, so a dependency that is down is found before anything mutates.
	Check func(ctx context.Context) error
}

// StepResult records one executed step.
type StepResult struct {
	Name    string
	Changed bool
}

// Bootstrap runs steps in order. It refuses before changing anything when any
// step is unavailable or any dependency check fails, so a partial bootstrap never leaves an install half
// provisioned. Re-running after success changes nothing.
func Bootstrap(ctx context.Context, steps []Step) ([]StepResult, error) {
	if err := Preflight(steps); err != nil {
		return nil, err
	}
	if err := Precheck(ctx, steps); err != nil {
		return nil, err
	}
	results := make([]StepResult, 0, len(steps))
	for _, s := range steps {
		changed, err := s.Run(ctx)
		if err != nil {
			return results, fmt.Errorf("bootstrap step %q: %w", s.Name, err)
		}
		results = append(results, StepResult{Name: s.Name, Changed: changed})
	}
	return results, nil
}

// Preflight reports every unavailable step without running anything.
func Preflight(steps []Step) error {
	var missing []string
	for _, s := range steps {
		if s.Issue != 0 {
			missing = append(missing, (&UnavailableError{What: s.Name, Issue: s.Issue}).Error())
		}
	}
	if len(missing) > 0 {
		return errors.New("bootstrap refused, nothing was changed: " + strings.Join(missing, "; "))
	}
	return nil
}

// Precheck runs every step's Check without mutating anything and reports all
// failures together.
func Precheck(ctx context.Context, steps []Step) error {
	var failed []string
	for _, s := range steps {
		if s.Check == nil {
			continue
		}
		if err := s.Check(ctx); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", s.Name, err))
		}
	}
	if len(failed) > 0 {
		return errors.New("bootstrap precheck failed, nothing was changed: " + strings.Join(failed, "; "))
	}
	return nil
}
