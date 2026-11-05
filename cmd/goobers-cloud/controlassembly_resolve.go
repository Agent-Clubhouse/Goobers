package main

import (
	"context"
	"errors"
	"log"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/startinput"
	"github.com/goobers/goobers/internal/instance"
)

// constructResolve shares Configuration's applied policy and lease-bound
// generation source with the controller runtime. Resolve reads the policy on
// each call: composition precedes the first epoch's configuration reload.
func constructResolve(_ context.Context, db databaseServices, _ controlAuth, _ *lease.Manager, _ func() *lease.Handle, p *controlPlanes) error {
	applied, ok := p.Configuration.(interface{ InstanceConfig() *instance.Config })
	if !ok {
		return errors.New("control start resolution requires the applied configuration")
	}
	if p.generations == nil {
		return errors.New("control start resolution requires the retained generation source")
	}
	if db.Pools == nil || db.Pools.Transaction == nil || db.InstanceID == "" {
		return errors.New("control start resolution requires the controller database and instance")
	}
	resolver := &startinput.Resolver{
		Generations: p.generations, Config: applied.InstanceConfig, InstanceID: db.InstanceID,
		Report: func(err error) { log.Printf("control start resolution: %v", err) },
	}
	p.Resolve = resolver.Resolve
	return nil
}
