package config

import (
	"context"
	"errors"
)

// Loader loads the candidate generation the control plane should run next.
type Loader func(ctx context.Context) (Candidate, error)

// Configuration is the control role's single owner of the Applier and the
// candidate loader. It satisfies startfence.GenerationAdmission (Admit) and
// control.Configuration (Admit plus Reload). One instance lives for the
// process, so the applied generation and its guard persist across lease
// epochs; a new epoch must not rebuild it.
type Configuration struct {
	applier *Applier
	load    Loader
}

// NewConfiguration returns a Configuration over assess, activator and loader.
func NewConfiguration(assess Assessor, activator Activator, load Loader) (*Configuration, error) {
	if assess == nil || activator == nil || load == nil {
		return nil, errors.New("cloud configuration requires an assessor, an activator and a loader")
	}
	return &Configuration{applier: NewApplier(assess, activator), load: load}, nil
}

// Admit refuses a start unless digest is the applied generation and it
// assessed gaggle/workflow eligible. It fails closed before anything applied.
func (c *Configuration) Admit(digest, gaggle, workflow string) error {
	return c.applier.Guard().Admit(digest, gaggle, workflow)
}

// Reload loads the next candidate and applies it atomically. A refused or
// failed candidate leaves the running generation and its guard untouched.
func (c *Configuration) Reload(ctx context.Context) error {
	candidate, err := c.load(ctx)
	if err != nil {
		return err
	}
	return c.applier.Apply(ctx, candidate)
}

// Status reports the applied generation and the latest refusal.
func (c *Configuration) Status() Status { return c.applier.Status() }

// Applied returns the running generation, if one has been applied.
func (c *Configuration) Applied() (Candidate, bool) { return c.applier.Applied() }
