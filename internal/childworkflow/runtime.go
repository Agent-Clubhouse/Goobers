package childworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// RuntimeConfig supplies only host-owned execution authority. Definitions is
// the successfully applied catalog, not an unvalidated working config tree.
type RuntimeConfig struct {
	Queue           *triggerqueue.Store
	Grants          *podauth.SignedKey
	Endpoint        string
	Definitions     *instance.ConfigSet
	OpenJournal     func(context.Context, string) (*journal.Reader, error)
	LoadPinnedStage func(context.Context, *instance.ConfigSet, journal.RunIdentity, string) (PinnedStageAdmission, error)
}

// Runtime fences grant issuance against applied policy reloads. It owns one
// current catalog snapshot and uses the existing bounded queue for all durable
// authority. Accepted child starts outlive individual tool sessions.
type Runtime struct {
	mu          sync.RWMutex
	definitions *instance.ConfigSet
	policies    map[string]string
	queue       *triggerqueue.Store
	resolver    *JournalAuthorityResolver
	issuer      GrantIssuer
	http        *HTTPService
}

// NewRuntime wires real journal resolution, grant issuance and HTTP submission.
// It does not enable the separately gated child execution backend.
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	if cfg.Queue == nil || cfg.Grants == nil || cfg.Definitions == nil || cfg.OpenJournal == nil || cfg.LoadPinnedStage == nil {
		return nil, ErrAuthorityUnavailable
	}
	if err := (&mcpio.ChildWorkflowAccess{Endpoint: cfg.Endpoint, BearerToken: "goobers-child.configuration-check"}).Validate("run-1"); err != nil {
		return nil, err
	}
	definitions := copyRuntimeDefinitions(cfg.Definitions)
	policies, err := childGagglePolicyDigests(definitions)
	if err != nil {
		return nil, err
	}
	r := &Runtime{definitions: definitions, policies: policies, queue: cfg.Queue}
	r.resolver = &JournalAuthorityResolver{
		OpenJournal: cfg.OpenJournal,
		LoadPinnedStage: func(ctx context.Context, id journal.RunIdentity, stage string) (PinnedStageAdmission, error) {
			return cfg.LoadPinnedStage(ctx, r.definitions, id, stage)
		},
	}
	r.issuer = GrantIssuer{Queue: cfg.Queue, Grants: cfg.Grants, Authority: r.resolver, Endpoint: cfg.Endpoint}
	r.http = &HTTPService{Grants: cfg.Grants, Submission: &SubmissionService{Queue: cfg.Queue, Authority: AuthorityResolverFunc(r.resolve)}}
	return r, nil
}

// HTTPService is the signed stage-only transport; anonymous loopback callers
// still need a valid stage grant.
func (r *Runtime) HTTPService() *HTTPService { return r.http }

func (r *Runtime) resolve(ctx context.Context, origin Origin) (Authority, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolver.Resolve(ctx, origin)
}

// Acquire registers the secret with the caller's run and instance scrubbers.
// Policy publication cannot interleave between preparation and durable binding.
func (r *Runtime) Acquire(ctx context.Context, env apiv1.InvocationEnvelope, secrets interface{ RegisterUntil([]byte, time.Time) }) (*mcpio.ChildWorkflowAccess, func() error, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	issuer := r.issuer
	issuer.Secrets = secrets
	return issuer.Acquire(ctx, env)
}

// AcquirePinnedStage lends the current policy snapshot to queued execution.
// The launcher must release after its bounded capacity/journal handoff, never
// hold this lease while the child executes or waits. It separately verifies
// parent cancellation and exact accepted source; this does not revive tool
// authority or authorize another stage to create a new child.
func (r *Runtime) AcquirePinnedStage(ctx context.Context, parent journal.RunIdentity, stage string) (PinnedStageAdmission, func(), error) {
	r.mu.RLock()
	var once sync.Once
	release := func() { once.Do(r.mu.RUnlock) }
	if err := ctx.Err(); err != nil {
		release()
		return PinnedStageAdmission{}, nil, err
	}
	pinned, err := r.resolver.LoadPinnedStage(ctx, parent, stage)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		release()
		return PinnedStageAdmission{}, nil, err
	}
	return pinned, release, nil
}

// ApplyDefinitions revokes tool grants for changed gaggles before publishing
// their new authority. publish must install the matching scheduler definitions
// or return an error without exposing them. A failed publication retains the
// old catalog, but already-revoked grants stay revoked. Unchanged gaggles keep
// their grants. New agent work in a changed gaggle needs a new journal attempt.
func (r *Runtime) ApplyDefinitions(ctx context.Context, next *instance.ConfigSet, publish func() error) error {
	if publish == nil {
		return ErrAuthorityUnavailable
	}
	if r == nil {
		return publish()
	}
	if next == nil {
		return ErrAuthorityUnavailable
	}
	definitions := copyRuntimeDefinitions(next)
	policies, err := childGagglePolicyDigests(definitions)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, gaggle := range slices.Sorted(maps.Keys(r.policies)) {
		if policies[gaggle] != r.policies[gaggle] {
			if err := r.queue.RevokeChildGaggleAuthorities(ctx, gaggle); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publish(); err != nil {
		return err
	}
	r.definitions, r.policies = definitions, policies
	return nil
}

func copyRuntimeDefinitions(set *instance.ConfigSet) *instance.ConfigSet {
	copy := *set
	copy.Manifest = set.Manifest.DeepCopy()
	copy.Gaggles = make([]apiv1.Gaggle, len(set.Gaggles))
	for i := range set.Gaggles {
		set.Gaggles[i].DeepCopyInto(&copy.Gaggles[i])
	}
	copy.Goobers = make([]apiv1.Goober, len(set.Goobers))
	for i := range set.Goobers {
		set.Goobers[i].DeepCopyInto(&copy.Goobers[i])
	}
	copy.Workflows = make([]apiv1.Workflow, len(set.Workflows))
	for i := range set.Workflows {
		set.Workflows[i].DeepCopyInto(&copy.Workflows[i])
	}
	return &copy
}

func childGagglePolicyDigests(set *instance.ConfigSet) (map[string]string, error) {
	result := make(map[string]string, len(set.Gaggles))
	for _, gaggle := range set.Gaggles {
		if _, duplicate := result[gaggle.Name]; duplicate || gaggle.Name == "" {
			return nil, errors.New("childworkflow: duplicate or unnamed authority gaggle")
		}
		goobers := make(map[string]apiv1.Goober)
		for _, goober := range set.Goobers {
			if goober.Spec.Gaggle == "" || goober.Spec.Gaggle == gaggle.Name {
				goobers[goober.Name] = goober
			}
		}
		workflows := make(map[string]apiv1.Workflow)
		for _, workflow := range set.Workflows {
			if workflow.Spec.Gaggle == gaggle.Name {
				workflows[workflow.Name] = workflow
			}
		}
		payload, err := json.Marshal(struct {
			Gaggle    apiv1.Gaggle
			Goobers   map[string]apiv1.Goober
			Workflows map[string]apiv1.Workflow
		}{gaggle, goobers, workflows})
		if err != nil {
			return nil, err
		}
		result[gaggle.Name] = digest(payload)
	}
	return result, nil
}
