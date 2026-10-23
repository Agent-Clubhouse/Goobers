// Package cutover implements the checks behind `goobers-cloud cutover check`
// and the steps behind `goobers-cloud bootstrap` (design §12.5, HA-P3-25).
//
// Anything that depends on a backend that has not landed fails closed with
// "not available until #N" rather than reporting a guess.
package cutover

import (
	"context"
	"errors"
	"fmt"
	"io"

	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/instance"
)

// SchemaVersion identifies the stable JSON report contract.
const SchemaVersion = "goobers.dev/cutover-check/v1"

// Status is the outcome of one check.
type Status string

const (
	// StatusPass means the check observed the required state.
	StatusPass Status = "pass"
	// StatusFail means the check observed a blocking state.
	StatusFail Status = "fail"
	// StatusUnavailable means the backing feature has not shipped; it blocks.
	StatusUnavailable Status = "unavailable"
)

// UnavailableError marks a capability whose backend has not shipped.
type UnavailableError struct {
	What  string
	Issue int
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("%s: not available until #%d", e.What, e.Issue)
}

// Install observes the target install's state.
type Install interface {
	SchemaCompatible(ctx context.Context) error
	LeaseRowPresent(ctx context.Context) (bool, error)
	AdmissionsPaused(ctx context.Context) (bool, error)
	BucketReachable(ctx context.Context) error
}

// Check is one install check result.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Report is the stable cutover-check result.
type Report struct {
	SchemaVersion string  `json:"schemaVersion"`
	Ready         bool    `json:"ready"`
	Config        Config  `json:"config"`
	Install       []Check `json:"install"`
	refusal       *cloudconfig.Refusal
}

// Config is the workflow-eligibility half of the report.
type Config struct {
	Eligible bool   `json:"eligible"`
	Summary  string `json:"summary,omitempty"`
}

// Run evaluates config eligibility and the install checks. install may be nil
// when no database is configured; every install check then fails.
func Run(ctx context.Context, assess cloudconfig.Assessor, cfg *instance.Config, set *instance.ConfigSet, install Install) Report {
	report := Report{SchemaVersion: SchemaVersion, Config: Config{Eligible: true}}
	if refusal := cloudconfig.Admit(assess, "cutover-check", cfg, set); refusal != nil {
		report.refusal = refusal
		report.Config = Config{Eligible: false, Summary: refusal.Error()}
	}
	report.Install = installChecks(ctx, install)
	report.Ready = report.Config.Eligible
	for _, c := range report.Install {
		report.Ready = report.Ready && c.Status == StatusPass
	}
	return report
}

func installChecks(ctx context.Context, install Install) []Check {
	type probe struct {
		name string
		run  func() (bool, string, error)
	}
	probes := []probe{
		{"schema compatible", func() (bool, string, error) {
			return true, "", install.SchemaCompatible(ctx)
		}},
		{"lease row present", func() (bool, string, error) {
			ok, err := install.LeaseRowPresent(ctx)
			return ok, "no lease row; run `goobers-cloud bootstrap`", err
		}},
		{"admissions paused", func() (bool, string, error) {
			ok, err := install.AdmissionsPaused(ctx)
			return ok, "admissions are not paused; pause before cutover", err
		}},
		{"bucket reachable", func() (bool, string, error) {
			return true, "", install.BucketReachable(ctx)
		}},
	}
	out := make([]Check, 0, len(probes))
	for _, p := range probes {
		if install == nil {
			out = append(out, Check{Name: p.name, Status: StatusFail, Detail: "no database configured"})
			continue
		}
		ok, failDetail, err := p.run()
		var unavailable *UnavailableError
		switch {
		case errors.As(err, &unavailable):
			out = append(out, Check{Name: p.name, Status: StatusUnavailable, Detail: err.Error()})
		case err != nil:
			out = append(out, Check{Name: p.name, Status: StatusFail, Detail: err.Error()})
		case !ok:
			out = append(out, Check{Name: p.name, Status: StatusFail, Detail: failDetail})
		default:
			out = append(out, Check{Name: p.name, Status: StatusPass})
		}
	}
	return out
}

// WriteText writes the operator-facing report.
func (r Report) WriteText(w io.Writer) error {
	var err error
	printf := func(format string, args ...any) {
		if err == nil {
			_, err = fmt.Fprintf(w, format, args...)
		}
	}
	if r.Config.Eligible {
		printf("config: cloud-ready\n")
	} else {
		printf("config: blocked\n")
		if r.refusal != nil && err == nil {
			err = r.refusal.WriteText(w)
		}
	}
	for _, c := range r.Install {
		printf("install: %-20s %s", c.Name, c.Status)
		if c.Detail != "" {
			printf(" (%s)", c.Detail)
		}
		printf("\n")
	}
	if r.Ready {
		printf("cutover check: ready\n")
	} else {
		printf("cutover check: blocked\n")
	}
	return err
}
