package main

import (
	"errors"
	"fmt"
	"time"

	cloudclaims "github.com/goobers/goobers/internal/cloud/claims"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/schedcounters"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/remediationselect"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/secretstore"
)

// scheduleDemandGate is the gate name of the schedule-demand counter, the one
// that can be unenforced while the others are enforced (it also needs the claims
// plane and the cross-run journal read).
const scheduleDemandGate = "schedule-demand"

// gateReporter is implemented by counters that can tell which gates they leave
// unenforced although they are composed.
type gateReporter interface {
	DisabledGates() map[string]string
}

// providerCounters is one generation's schedcounters with the gates it leaves
// unenforced, so readiness never claims more than is enforced.
type providerCounters struct {
	*schedcounters.Counters
	disabled map[string]string
}

var _ generationCounters = (*providerCounters)(nil)
var _ gateReporter = (*providerCounters)(nil)

// DisabledGates names the gates this generation's counters do not enforce.
func (c *providerCounters) DisabledGates() map[string]string { return c.disabled }

// counterCredentials resolves a repository's token reference ("owner/name") the
// way the local daemon's counters do: an instance-level credential resolver over
// the applied configuration's repos, backed by its secret stores. A github-app
// repo mints short-lived installation tokens under the same reference, so a
// resolve is made per poll and never cached by the caller.
type counterCredentials func(cfg *instance.Config) (credentials.Resolver, error)

// newProviderCounters returns the control role's Counters factory: GitHub-backed,
// like the local daemon's demand counters (its entries charge the GitHub provider
// regardless of the repository). A repository on another provider fails its poll
// with a clear error, which leaves its open-PR count unknown (the gate fails open
// for it) and its backlog counter without work; it never opens a client with the
// wrong credential.
//
// claims and journal feed the schedule-demand counter; when either is absent the
// counter is not built, schedentries keeps those workflows disabled with their
// pending reason, and the gate stays reported as not enforced.
func newProviderCounters(credentialsFor counterCredentials, claims remediationselect.NamespaceLister, journalReader remediationselect.BranchOwnershipReader, registrar credentials.SecretRegistrar, report func(error)) func(cloudconfig.Candidate, *localscheduler.ProviderQuotaState) (generationCounters, error) {
	return func(applied cloudconfig.Candidate, quota *localscheduler.ProviderQuotaState) (generationCounters, error) {
		cfg, set := applied.Config, applied.Set
		if cfg == nil || set == nil {
			return nil, ErrCountersDisabled
		}
		resolver, err := credentialsFor(cfg)
		if err != nil {
			return nil, fmt.Errorf("provider counters: credentials: %w", err)
		}
		source := schedcounters.GitHubSource{Resolve: resolver.Resolve, Register: registrar.Register}
		deps := schedcounters.Deps{
			Config: cfg, Set: set, Open: source.Open, Quota: quota,
			Now: time.Now, Report: report,
		}
		disabled := map[string]string{}
		if claims != nil && journalReader != nil {
			deps.Claims, deps.Journal = claims, journalReader
		} else {
			disabled[scheduleDemandGate] = "the schedule-demand counter needs the claims plane and the cross-run journal read, which are not composed"
		}
		counters, err := schedcounters.New(deps)
		switch {
		case errors.Is(err, schedcounters.ErrNoOpener):
			return nil, ErrCountersDisabled
		case err != nil:
			return nil, fmt.Errorf("provider counters: %w", err)
		}
		return &providerCounters{Counters: counters, disabled: disabled}, nil
	}
}

// productionCounterCredentials builds the instance-level resolver from the applied
// configuration's secret stores.
func productionCounterCredentials(registrar credentials.SecretRegistrar) counterCredentials {
	return func(cfg *instance.Config) (credentials.Resolver, error) {
		stores, err := secretstore.NewRegistry(cfg.SecretStores)
		if err != nil {
			return nil, err
		}
		resolver, _, err := runtimewiring.BuildCredentials(cfg, stores, "", "", nil, registrar)
		return resolver, err
	}
}

// compositionCounters wires the production factory from the control planes; it
// returns nil (counters stay unenforced and reported) when the planes it feeds
// on are absent.
func compositionCounters(p *controlPlanes, report func(error)) func(cloudconfig.Candidate, *localscheduler.ProviderQuotaState) (generationCounters, error) {
	registrar := journal.NewRegistryScrubber()
	var claims remediationselect.NamespaceLister
	if plane, ok := p.Claims.(*cloudclaims.Plane); ok {
		claims = plane
	}
	var reader remediationselect.BranchOwnershipReader
	if r, ok := p.RunJournal.(remediationselect.BranchOwnershipReader); ok {
		reader = r
	}
	return newProviderCounters(productionCounterCredentials(registrar), claims, reader, registrar, report)
}
