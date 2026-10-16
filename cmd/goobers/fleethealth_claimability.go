package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/claimability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/providerconfig"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// Claimability observation bounds per retained poll: candidates kept from the
// poll page, provider-metered shared lease reads, total duration, and the
// bytes read from each local state file (the state plane's own value cap).
const (
	backlogClaimCandidateLimit  = 20
	backlogClaimSharedReadLimit = 10
	backlogClaimTimeout         = 5 * time.Second
	backlogClaimSourceMaxBytes  = stateclient.MaxValueBytes
)

// backlogClaimTarget is the admission identity of a counter's polled items.
type backlogClaimTarget struct {
	policy    claimability.Policy
	window    time.Duration
	maxWindow time.Duration
	repo      providers.RepositoryRef
}

type backlogClaimTargetReader interface {
	backlogClaimTarget() (backlogClaimTarget, bool)
}

func (b *backlogCounter) backlogClaimTarget() (backlogClaimTarget, bool) {
	if b.claim == nil {
		return backlogClaimTarget{}, false
	}
	return *b.claim, true
}

// newBacklogClaimTarget mirrors the workflow's backlog-query admission: its
// gaggle namespace, the polled provider, the configured claim visibility and
// the shortest and longest leases its disagreement backoff could use. A
// workflow whose admission identity cannot be derived statically, including a
// gaggle whose backlog admission keys on another provider, yields no target,
// which keeps its pending work claimability_unknown.
func newBacklogClaimTarget(wf *apiv1.Workflow, repo providers.RepositoryRef, backlogProvider apiv1.Provider) *backlogClaimTarget {
	if providerconfig.CrossProviderBacklog(apiv1.Provider(repo.Provider), backlogProvider) {
		return nil
	}
	shared := false
	switch wf.Spec.Readiness.ClaimVisibility {
	case "", "local":
	case "shared":
		if repo.Provider != providers.ProviderGitHub {
			return nil
		}
		shared = true
	default:
		return nil
	}
	var shortest, longest time.Duration
	for _, task := range wf.Spec.Tasks {
		if task.Run == nil || len(task.Run.Command) < 2 || task.Run.Command[0] != "goobers" || task.Run.Command[1] != "backlog-query" {
			continue
		}
		lease := DefaultClaimLease
		if raw := task.Inputs["leaseDuration"]; raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil || parsed <= 0 {
				return nil
			}
			lease = parsed
		}
		if shortest == 0 || lease < shortest {
			shortest = lease
		}
		longest = max(longest, lease)
	}
	if shortest == 0 || wf.Spec.Gaggle == "" {
		return nil
	}
	return &backlogClaimTarget{policy: claimability.Policy{Gaggle: wf.Spec.Gaggle, Provider: string(repo.Provider), Shared: shared}, window: shortest, maxWindow: longest, repo: repo}
}

func (o *backlogPollObservation) retainCandidate(item providers.WorkItem) {
	if len(o.candidates) >= backlogClaimCandidateLimit {
		o.truncated = true
		return
	}
	o.candidates = append(o.candidates, claimability.Candidate{ID: item.ID, ProviderClaimed: item.HasLabel(providers.LabelClaimed)})
}

type backlogClaimProbe func(context.Context, time.Time, backlogClaimTarget, backlogPollObservation) claimability.Result

type backlogClaimCache struct {
	observedAt time.Time
	result     claimability.Result
}

// claimability classifies a fresh poll at most once: repeated samples of the
// same poll reuse its result, so provider reads stay bounded per poll.
func (s *backlogHealthSampler) claimability(ctx context.Context, source backlogObservationReader, evidence backlogPollObservation) (claimability.Result, bool) {
	reader, ok := source.(backlogClaimTargetReader)
	if !ok || s.probe == nil {
		return claimability.Result{}, false
	}
	target, ok := reader.backlogClaimTarget()
	if !ok {
		return claimability.Result{}, false
	}
	if cached, ok := s.claims[source]; ok && cached.observedAt.Equal(evidence.observedAt) {
		return cached.result, true
	}
	result := s.probe(ctx, time.Now().UTC(), target, evidence)
	if s.claims == nil {
		s.claims = map[backlogObservationReader]backlogClaimCache{}
	}
	s.claims[source] = backlogClaimCache{observedAt: evidence.observedAt, result: result}
	return result, true
}

func (s *backlogHealthSampler) pruneClaims(sources map[localscheduler.WorkflowIdentity]backlogObservationReader) {
	live := map[backlogObservationReader]bool{}
	for _, source := range sources {
		live[source] = true
	}
	for source := range s.claims {
		if !live[source] {
			delete(s.claims, source)
		}
	}
}

// daemonBacklogClaimProbe reads the same claim ledger, blocked records and
// shared leases admission consults, without taking their write locks or
// writing to any of them.
func daemonBacklogClaimProbe(setup *schedulerSetup) backlogClaimProbe {
	layout := instance.NewLayout(setup.Root)
	return func(ctx context.Context, now time.Time, target backlogClaimTarget, evidence backlogPollObservation) claimability.Result {
		sources := claimability.Sources{
			Local: func(ctx context.Context) (claimability.LocalSnapshot, error) {
				return readClaimabilityLedger(ctx, layout)
			},
			Blocked: func(ctx context.Context) (claimability.BlockedSnapshot, error) {
				return readClaimabilityBlocked(ctx, layout, target.repo)
			},
			Shared: func(ctx context.Context) (sharedclaim.Store, error) {
				return observationSharedClaimStore(ctx, setup, target.repo)
			},
		}
		limits := claimability.Limits{MaxCandidates: backlogClaimCandidateLimit, MaxSharedReads: backlogClaimSharedReadLimit, Timeout: backlogClaimTimeout, DisagreementWindow: target.window, DisagreementMaxWindow: target.maxWindow}
		return claimability.Observe(ctx, now, target.policy, evidence.candidates, evidence.truncated || !evidence.complete, sources, limits)
	}
}

// readClaimabilityLedger reads the ledger file read-only within the byte cap
// and the observation deadline; the ledger replaces it atomically, so an
// unlocked read is one consistent revision.
func readClaimabilityLedger(ctx context.Context, layout instance.Layout) (claimability.LocalSnapshot, error) {
	path := filepath.Join(layout.SchedulerDir(), claimLedgerFileName)
	data, err := claimability.ReadFile(ctx, path, backlogClaimSourceMaxBytes)
	if err != nil {
		return claimability.LocalSnapshot{}, err
	}
	ledger, err := localscheduler.ParseClaimLedger(path, data)
	if err != nil {
		return claimability.LocalSnapshot{}, err
	}
	return claimability.LocalSnapshot{Entries: ledger.Snapshot(), History: ledger.HistorySnapshot()}, nil
}

// readClaimabilityBlocked classifies learned dependency blocks for the polled
// repository from the same file the held state store serves, read within the
// byte cap and the observation deadline. A record for the same repository
// name under a different or missing scope is treated as unscoped: admission
// may migrate or apply it.
func readClaimabilityBlocked(ctx context.Context, layout instance.Layout, repo providers.RepositoryRef) (claimability.BlockedSnapshot, error) {
	relative, err := stateclient.KeyRelativePath(stateclient.KeyBlockedRecords)
	if err != nil {
		return claimability.BlockedSnapshot{}, err
	}
	data, err := claimability.ReadFile(ctx, filepath.Join(layout.SchedulerDir(), relative), backlogClaimSourceMaxBytes)
	if err != nil {
		return claimability.BlockedSnapshot{}, err
	}
	value := stateclient.Value{}
	if data != nil {
		value = stateclient.Value{Data: data, ETag: stateclient.ETagFor(data)}
	}
	records, err := decodeBlockedRecords(value)
	if err != nil {
		return claimability.BlockedSnapshot{}, err
	}
	snapshot := claimability.BlockedSnapshot{Scoped: map[string]struct{}{}, Unscoped: map[string]struct{}{}}
	for key, record := range records {
		id := blockedRecordItemID(key, record)
		switch {
		case blockedRecordAppliesToRepository(record, repo):
			snapshot.Scoped[id] = struct{}{}
		case blockedRepositoryEmpty(record.Repository) || record.Repository.Provider == repo.Provider && record.Repository.Owner == repo.Owner && record.Repository.Name == repo.Name:
			snapshot.Unscoped[id] = struct{}{}
		}
	}
	return snapshot, nil
}

// observationSharedClaimStore opens the daemon's shared lease store for the
// polled repository with every request reserved through provider quota and no
// rate-limit or transient retries. Without quota accounting no read is made.
func observationSharedClaimStore(ctx context.Context, setup *schedulerSetup, polled providers.RepositoryRef) (sharedclaim.Store, error) {
	if setup.ProviderQuota == nil || setup.SharedRegistry == nil || setup.SecretStores == nil || setup.Config == nil {
		return nil, errors.New("shared claim observation is not configured")
	}
	var repo *providers.RepositoryRef
	for _, configured := range setup.Config.Repos {
		if providers.ProviderKind(configured.Provider) != polled.Provider || configured.Owner != polled.Owner || configured.Name != polled.Name || configured.Project != polled.Project {
			continue
		}
		if repo != nil {
			return nil, errors.New("shared claim observation repository is ambiguous")
		}
		repo = &providers.RepositoryRef{Provider: polled.Provider, URL: configured.BaseURL, Owner: configured.Owner, Project: configured.Project, Name: configured.Name}
	}
	if repo == nil {
		return nil, errors.New("shared claim observation repository is not configured")
	}
	provider, err := daemonSharedClaimProvider(ctx, setup.Config, *repo, setup.SharedRegistry, setup.SecretStores, capability.RepoPush)
	if err != nil {
		return nil, err
	}
	accounting := &providerQuotaAccounting{state: setup.ProviderQuota}
	for _, option := range []func(*providers.GitHubProvider){
		providers.WithQuotaObserver(accounting), providers.WithQuotaRequestGate(accounting),
		providers.WithMaxRateLimitRetries(0), providers.WithMaxTransientRetries(0),
	} {
		option(provider)
	}
	return scrubbedSharedClaimStore{store: providers.GitHubSharedClaimStore{Provider: provider, Repository: *repo}, registrar: setup.SharedRegistry}, nil
}
