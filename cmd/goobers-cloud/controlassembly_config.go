package main

import (
	"context"
	"errors"
	"os"

	"github.com/goobers/goobers/internal/childworkflow"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/config/readiness"
	"github.com/goobers/goobers/internal/cloud/generationpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/objstore"
	"github.com/goobers/goobers/internal/cloud/startpin"
)

// constructConfiguration composes the control role's single configuration owner:
// a mounted, read-only instance directory is captured with the daemon's
// configgeneration builder, assessed for engine-only eligibility, and published
// to the object-store generation pointer under the live controller lease. A
// refused candidate leaves the running generation applied.
func constructConfiguration(_ context.Context, db databaseServices, _ controlAuth, manager *lease.Manager, _ func() *lease.Handle, p *controlPlanes) error {
	if db.ConfigDir == "" {
		return errors.New("control configuration requires --config-dir (or GOOBERS_CONFIG_DIR)")
	}
	if db.Objects == nil || db.InstanceID == "" {
		return errors.New("control configuration requires the object store (--object-store-url) and instance")
	}
	if manager == nil || db.EpochHandle == nil {
		return errors.New("control configuration requires the controller lease")
	}
	activator, err := generationpg.NewActivator(db.Objects, db.InstanceID)
	if err != nil {
		return err
	}
	configuration, err := cloudconfig.NewDirConfiguration(readiness.Assess, leasedActivator{inner: activator, manager: manager, owner: db.EpochHandle}, cloudconfig.DirSource{Root: db.ConfigDir, InstanceID: db.InstanceID, Pins: startpin.Compute})
	if err != nil {
		return err
	}
	source, err := objstore.NewGenerationSource(db.Objects, db.InstanceID, "")
	if err != nil {
		return err
	}
	p.Configuration = configuration
	p.generations = leasedGenerations{inner: source, owner: db.EpochHandle}
	return nil
}

// leasedActivator binds the activator's fence to the epoch handle that is live
// at the moment of the apply. Reload runs inside an acquired epoch, during
// rebuild-before-reap, before the readiness-gated owner exists, so it takes the
// epoch handle. A missing handle means authority was lost and nothing may be
// published.
type leasedActivator struct {
	inner   *generationpg.Activator
	manager *lease.Manager
	owner   func() *lease.Handle
}

func (a leasedActivator) Activate(ctx context.Context, candidate cloudconfig.Candidate) error {
	handle := a.owner()
	if handle == nil {
		return lease.ErrStale
	}
	a.inner.BindFence(a.manager, handle)
	return a.inner.Activate(ctx, candidate)
}

// leasedGenerations reads retained generations under the live controller epoch:
// the read pin is fenced, so each acquire binds the handle that is live now. The
// later composition seams (child admissions, start resolution) share it.
type leasedGenerations struct {
	inner *objstore.Source
	owner func() *lease.Handle
}

func (g leasedGenerations) Acquire(ctx context.Context, generation string) (childworkflow.Generation, error) {
	handle := g.owner()
	if handle == nil {
		return childworkflow.Generation{}, lease.ErrStale
	}
	g.inner.BindFence(handle)
	return g.inner.Acquire(ctx, generation)
}

var _ childworkflow.GenerationSource = leasedGenerations{}

// validateControlConfigDir refuses to boot without a mounted config directory:
// a control plane with no configuration source can never admit anything. It
// runs before any database is opened so the operator sees the real cause.
func validateControlConfigDir(dir string) error {
	if dir == "" {
		return errors.New("--config-dir (or GOOBERS_CONFIG_DIR) is required: the control role admits only from a mounted instance config directory")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return errors.New("config directory is not readable: " + dir)
	}
	if !info.IsDir() {
		return errors.New("config directory is not a directory: " + dir)
	}
	return nil
}

// bindControlConfiguration gives the controller role its mounted config
// directory, rechecked at bind time.
func bindControlConfiguration(controller *databaseServices, dir string) error {
	if err := validateControlConfigDir(dir); err != nil {
		return err
	}
	controller.ConfigDir = dir
	return nil
}
