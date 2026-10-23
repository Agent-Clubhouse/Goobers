package config

import (
	"context"
	"maps"
	"sync"

	"github.com/goobers/goobers/internal/instance"
)

// Candidate is a loaded configuration generation awaiting activation.
type Candidate struct {
	Digest string
	// Archive is the content-addressed execution generation retained on apply.
	Archive []byte
	Config  *instance.Config
	Set     *instance.ConfigSet
	// Pins are the admission digests of the candidate's workflows, keyed
	// "gaggle/workflow". Optional: without them the applied generation pins no
	// start (PinFor fails closed).
	Pins map[string]Pins
}

// Activator switches the running generation. The concrete implementation is
// backed by the object-store generation pointer (HA-P3-13); admission only
// needs the switch.
type Activator interface {
	Activate(ctx context.Context, candidate Candidate) error
}

// Status is what `config status` reports.
type Status struct {
	// Applied is the digest of the running generation, empty before the first apply.
	Applied string
	// RejectedDigest and Refusal describe the most recent refused candidate;
	// both are cleared by a successful apply.
	RejectedDigest string
	Refusal        *Refusal
}

// Applier gates generation activation on engine-only eligibility and keeps the
// scheduler guard bound to exactly the generation that was activated.
type Applier struct {
	assess    Assessor
	activator Activator
	guard     *Guard

	// mu serialises the whole assess→activate→bind→status transition, so
	// Status and the guard always describe the generation actually active.
	mu     sync.Mutex
	status Status
}

// NewApplier returns an Applier that activates through activator.
func NewApplier(assess Assessor, activator Activator) *Applier {
	return &Applier{assess: assess, activator: activator, guard: NewGuard()}
}

// Guard returns the scheduler guard bound to the running generation.
func (a *Applier) Guard() *Guard { return a.guard }

// Apply activates the candidate only when it is eligible. A refused or failed
// candidate leaves the running generation and its guard untouched.
func (a *Applier) Apply(ctx context.Context, c Candidate) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	gen := Evaluate(a.assess, c.Digest, c.Config, c.Set)
	if refusal := gen.Refusal(); refusal != nil {
		a.status.RejectedDigest, a.status.Refusal = c.Digest, refusal
		return refusal
	}
	if err := a.activator.Activate(ctx, c); err != nil {
		return err
	}
	gen.pins, gen.cfg = maps.Clone(c.Pins), c.Config
	a.guard.Replace(gen)
	a.status = Status{Applied: c.Digest}
	return nil
}

// Status returns a snapshot of the apply state.
func (a *Applier) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}
