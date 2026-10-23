// Package config decides whether a configuration generation may run on the
// engine-only cloud control plane. The cloud has no runner fallback (D-2), so
// a generation that contains a workflow the engine cannot execute is refused
// whole rather than half-applied.
//
// This package deliberately depends on no execution code: the workflow
// assessment arrives through an Assessor, and the production one lives in
// the readiness subpackage.
package config

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/goobers/goobers/internal/instance"
)

// ErrIneligible classifies every admission refusal.
var ErrIneligible = errors.New("configuration is not eligible for the engine-only cloud")

// Finding is one reason a workflow cannot run on the engine-only cloud.
type Finding struct {
	// Code is RUNTIME001, RUNTIME002 or RUNTIME003.
	Code    string
	Stage   string
	Path    string
	Message string
	// Unblock is the operator action that removes the finding.
	Unblock string
}

// Assessment maps "gaggle/workflow" to its findings. A workflow with no entry
// is eligible.
type Assessment map[string][]Finding

// Assessor evaluates every workflow of a loaded configuration.
type Assessor func(cfg *instance.Config, set *instance.ConfigSet) Assessment

// Refusal is the actionable reason a generation was not applied.
type Refusal struct {
	// Policy is non-empty when the generation cannot be assessed at all or its
	// placement policy is invalid.
	Policy string
	// Workflows holds every blocked workflow's findings.
	Workflows Assessment
}

// Error summarizes the refusal; WriteText prints the per-blocker detail.
func (r *Refusal) Error() string {
	if r.Policy != "" {
		return fmt.Sprintf("%v: %s", ErrIneligible, r.Policy)
	}
	total := 0
	for _, findings := range r.Workflows {
		total += len(findings)
	}
	return fmt.Sprintf("%v: blocked: %d workflows, %d blockers", ErrIneligible, len(r.Workflows), total)
}

// Unwrap lets callers classify with errors.Is.
func (r *Refusal) Unwrap() error { return ErrIneligible }

// WriteText writes the operator-facing blocker report for `config status`.
func (r *Refusal) WriteText(w io.Writer) error {
	if r.Policy != "" {
		_, err := fmt.Fprintln(w, r.Policy)
		return err
	}
	names := make([]string, 0, len(r.Workflows))
	for name := range r.Workflows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := fmt.Fprintf(w, "%s\n", name); err != nil {
			return err
		}
		for _, f := range r.Workflows[name] {
			stage := ""
			if f.Stage != "" {
				stage = fmt.Sprintf(" stage %q", f.Stage)
			}
			if _, err := fmt.Fprintf(w, "  - %s%s %s: %s\n    unblock: %s\n", f.Code, stage, f.Path, f.Message, f.Unblock); err != nil {
				return err
			}
		}
	}
	return nil
}

// Generation is an immutable assessed configuration. Eligible is the only
// source of truth for what the scheduler may start under this generation.
type Generation struct {
	digest   string
	eligible map[string]struct{}
	refusal  *Refusal
}

// Digest identifies the generation the assessment was computed for.
func (g *Generation) Digest() string { return g.digest }

// Refusal is nil only for a generation that may be applied whole.
func (g *Generation) Refusal() *Refusal { return g.refusal }

func workflowKey(gaggle, name string) string { return gaggle + "/" + name }

// Evaluate assesses a candidate generation. It never returns nil. Absent
// loaded evidence (nil config, nil set or no assessor) refuses and admits
// nothing: missing evidence never establishes eligibility.
func Evaluate(assess Assessor, digest string, cfg *instance.Config, set *instance.ConfigSet) *Generation {
	g := &Generation{digest: digest, eligible: map[string]struct{}{}}
	switch {
	case digest == "":
		g.refusal = &Refusal{Policy: "generation digest is required"}
		return g
	case cfg == nil || set == nil || assess == nil:
		g.refusal = &Refusal{Policy: "no loaded configuration to assess"}
		return g
	case cfg.SelfExecutionPolicy() != "deny":
		g.refusal = &Refusal{Policy: fmt.Sprintf(
			"placement.selfExecution: cloud requires %q (got %q); the control plane never executes stages",
			"deny", cfg.SelfExecutionPolicy())}
		return g
	}
	blocked := assess(cfg, set)
	for i := range set.Workflows {
		w := &set.Workflows[i]
		if key := workflowKey(w.Spec.Gaggle, w.Name); len(blocked[key]) == 0 {
			g.eligible[key] = struct{}{}
		}
	}
	// A finding for a workflow the set does not contain still blocks.
	if len(blocked) > 0 {
		g.refusal = &Refusal{Workflows: blocked}
	}
	return g
}

// Admit reports whether the candidate may be applied whole.
func Admit(assess Assessor, digest string, cfg *instance.Config, set *instance.ConfigSet) *Refusal {
	return Evaluate(assess, digest, cfg, set).refusal
}
