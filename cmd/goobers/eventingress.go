package main

import (
	"context"
	"net/http"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/eventingress"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

func (u *upSession) configureEventIngress() {
	if u.durableTriggers == nil || u.setup.EventPublisher == nil || u.setup.InteractiveAccess == nil {
		return
	}
	service := &eventingress.Service{Queue: u.durableTriggers.queue, Authorize: u.setup.EventPublisher.authorizeIngress, View: u.setup.InteractiveAccess, Scrubber: journal.Chain(u.setup.SharedRegistry, journal.NewPatternScrubber())}
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithGaggleEvents(service))
}

// An external binding grants no run identity. Applied policy and its retained
// catalog remain locked until the shared ledger commits the matched receipt.
func (p *daemonEventPublisher) authorizeIngress(ctx context.Context, principal httpapi.Principal, gaggle, name string, accept func(apiv1.EventIngressBinding, *eventing.Catalog) error) error {
	if err := p.lockIngressSnapshot(ctx); err != nil {
		return err
	}
	defer p.mu.RUnlock()
	denied := httpapi.NewInterventionError(http.StatusForbidden, "event_scope_denied", "Event producer binding is not authorized.", nil)
	policy := p.snapshot.policies[gaggle]
	if policy == nil {
		return denied
	}
	var binding *apiv1.EventIngressBinding
	for i := range policy.Ingress {
		candidate := &policy.Ingress[i]
		if candidate.Name == name && candidate.Issuer == principal.Issuer && candidate.Subject == principal.Subject {
			binding = candidate
			break
		}
	}
	if binding == nil {
		return denied
	}
	store, err := executionGenerationStore(p.layout)
	if err != nil {
		return err
	}
	_, lease, err := store.Acquire(ctx, p.snapshot.generation)
	if err != nil {
		return err
	}
	defer func() { _ = lease.Release() }()
	return accept(*binding.DeepCopy(), p.snapshot.catalogs[gaggle])
}

// A reload can hold the publisher write lock while it installs the applied
// archive. Waiting for that lock must honor the listener request budget.
func (p *daemonEventPublisher) lockIngressSnapshot(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.mu.TryRLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
