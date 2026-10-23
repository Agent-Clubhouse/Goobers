// Package chaos is the HA fault-injection framework: injectors, a scenario
// registry and an evidence bundle writer. Scenarios that need components not
// yet merged are declared with a Pending reference and reported as skipped
// with that reason; they never pass silently.
package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Env is what a scenario receives. Fields are seams the nightly wiring fills.
type Env struct {
	Exec      Executor
	Toxiproxy *Toxiproxy
	// Check runs the invariant checker against the system under test and
	// returns the violation count.
	Check func(ctx context.Context) (violations int, err error)
	// Report records a reported-not-asserted measurement (e.g. an RTO).
	Report func(name string, d time.Duration)
}

// Scenario is one chaos scenario. Exactly one of Run and Pending is set.
type Scenario struct {
	ID          string
	Description string
	// Pending is "pending #<PR>" for a scenario whose components are unmerged.
	Pending string
	Run     func(ctx context.Context, env Env) error
}

var pendingForm = regexp.MustCompile(`^pending #[0-9]+$`)

// Validate rejects scenarios that could pass silently.
func (s Scenario) Validate() error {
	switch {
	case s.ID == "":
		return errors.New("scenario has no ID")
	case s.Run == nil && s.Pending == "":
		return fmt.Errorf("%s has neither Run nor Pending", s.ID)
	case s.Run != nil && s.Pending != "":
		return fmt.Errorf("%s has both Run and Pending", s.ID)
	case s.Pending != "" && !pendingForm.MatchString(s.Pending):
		return fmt.Errorf("%s pending reason %q must match %q", s.ID, s.Pending, pendingForm)
	}
	return nil
}

// Status is a scenario outcome.
type Status string

// Scenario outcomes.
const (
	Passed  Status = "passed"
	Failed  Status = "failed"
	Skipped Status = "skipped"
)

// Result is one scenario's outcome.
type Result struct {
	ID       string `json:"id"`
	Status   Status `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Duration string `json:"duration"`
}

// Summary is the run outcome written into the evidence bundle.
type Summary struct {
	Results  []Result          `json:"results"`
	Reported map[string]string `json:"reported,omitempty"`
}

// Counts returns passed, failed and skipped totals.
func (s Summary) Counts() (passed, failed, skipped int) {
	for _, r := range s.Results {
		switch r.Status {
		case Passed:
			passed++
		case Failed:
			failed++
		case Skipped:
			skipped++
		}
	}
	return
}

// RunAll runs scenarios in ID order. A scenario passes only if Run returns nil
// and the invariant checker (when wired) reports no violations.
func RunAll(ctx context.Context, scenarios []Scenario, env Env) (Summary, error) {
	sorted := append([]Scenario(nil), scenarios...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	summary := Summary{Reported: map[string]string{}}
	prevReport := env.Report
	env.Report = func(name string, d time.Duration) {
		summary.Reported[name] = d.String()
		if prevReport != nil {
			prevReport(name, d)
		}
	}
	for _, s := range sorted {
		if err := s.Validate(); err != nil {
			return summary, err
		}
		start := time.Now()
		res := Result{ID: s.ID}
		if s.Pending != "" {
			res.Status, res.Reason = Skipped, s.Pending
		} else if err := runOne(ctx, s, env); err != nil {
			res.Status, res.Reason = Failed, err.Error()
		} else {
			res.Status = Passed
		}
		res.Duration = time.Since(start).Round(time.Millisecond).String()
		summary.Results = append(summary.Results, res)
	}
	return summary, nil
}

func runOne(ctx context.Context, s Scenario, env Env) error {
	if err := s.Run(ctx, env); err != nil {
		return err
	}
	if env.Check == nil {
		return errors.New("no invariant checker wired; refusing to pass unchecked")
	}
	n, err := env.Check(ctx)
	if err != nil {
		return fmt.Errorf("invariant check: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%d invariant violation(s)", n)
	}
	return nil
}

// WriteEvidence writes the summary as chaos-summary.json under dir.
func (s Summary) WriteEvidence(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "chaos-summary.json"), data, 0o644)
}
