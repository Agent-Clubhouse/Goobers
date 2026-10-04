package interactiveaccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// Dependencies reuses the daemon's secret stores, redaction registry and ADO
// identity command runner. No automation credential resolver is accepted.
type Dependencies struct {
	Stores    credentials.StoreResolver
	Registrar credentials.SecretRegistrar
	Runner    providers.CommandRunner
}

// Service fences each credential use against the currently applied policy.
// An operation callback must complete its bounded provider effect before
// returning and must not retain a credential or start asynchronous effects.
type Service struct {
	executionMu           sync.Mutex
	executions            map[*ExecutionLease]struct{}
	executionDrainTimeout time.Duration
	stageRestartAvailable atomic.Bool
	mu                    sync.RWMutex
	gaggles               map[string]*apiv1.Gaggle
	sources               map[string]instance.InteractiveCredential
	deps                  Dependencies
	sourceMu              sync.Mutex
	resolved              map[string]resolvedSource
}

// New pins configuration without resolving or minting secrets.
func New(gaggles []apiv1.Gaggle, sources []instance.InteractiveCredential, deps Dependencies) (*Service, error) {
	if deps.Registrar == nil {
		return nil, errors.New("interactive credential redaction registry is required")
	}
	pinned, err := pinGaggles(gaggles)
	if err != nil {
		return nil, err
	}
	// Token/auth source structs contain pointers; copy them as well as policy.
	raw, err := json.Marshal(sources)
	if err != nil {
		return nil, err
	}
	var copied []instance.InteractiveCredential
	if err := json.Unmarshal(raw, &copied); err != nil {
		return nil, err
	}
	named := make(map[string]instance.InteractiveCredential, len(copied))
	for _, source := range copied {
		if source.Name == "" {
			return nil, errors.New("interactive credential name is required")
		}
		if _, exists := named[source.Name]; exists {
			return nil, errors.New("interactive credential names must be unique")
		}
		named[source.Name] = source
	}
	return &Service{gaggles: pinned, sources: named, deps: deps, resolved: make(map[string]resolvedSource)}, nil
}

func pinGaggles(gaggles []apiv1.Gaggle) (map[string]*apiv1.Gaggle, error) {
	pinned := make(map[string]*apiv1.Gaggle, len(gaggles))
	for i := range gaggles {
		g := gaggles[i].DeepCopy()
		if g.Name == "" || pinned[g.Name] != nil {
			return nil, errors.New("interactive gaggle identities must be nonempty and unique")
		}
		if err := validatePolicy(g.Spec.InteractiveAccess); err != nil {
			return nil, fmt.Errorf("gaggle %s: %w", g.Name, err)
		}
		pinned[g.Name] = g
	}
	return pinned, nil
}

// Apply publishes a catalog while credential operations are fenced. A failed
// publication leaves the previous applied policy intact. publish must not call
// back into this service; the lock order is interactive policy before scheduler.
func (s *Service) Apply(gaggles []apiv1.Gaggle, publish func() error) error {
	next, err := pinGaggles(gaggles)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cancelChangedExecutions(next); err != nil {
		return err
	}
	if publish != nil {
		if err := publish(); err != nil {
			return err
		}
	}
	s.gaggles = next
	return nil
}

// Authorize checks the current instance role and explicit gaggle action grant.
// This is a snapshot check, not an execution grant. Effects must use
// WithAuthorization or WithCredential so policy reload cannot race acceptance.
func (s *Service) Authorize(p httpapi.Principal, gaggle string, action apiv1.InteractiveAction) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return authorize(p, s.gaggles[gaggle], action)
}

// WithAuthorization fences a bounded non-provider command acceptance against
// policy reload. Provider effects must use WithCredential for exact target
// selection. Accepted asynchronous work must retain and recheck its own ceiling.
func (s *Service) WithAuthorization(ctx context.Context, p httpapi.Principal, gaggle string, action apiv1.InteractiveAction, use func(context.Context) error) error {
	if use == nil || providerAction(action) {
		return ErrDenied
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := authorize(p, s.gaggles[gaggle], action); err != nil {
		return err
	}
	return use(ctx)
}

// InteractiveCapabilities implements the authenticated permission route.
func (s *Service) InteractiveCapabilities(ctx context.Context, p httpapi.Principal, gaggle string) (apicontract.InteractiveCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return apicontract.InteractiveCapabilities{}, err
	}
	if !human(p) {
		return apicontract.InteractiveCapabilities{}, ErrDenied
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := s.gaggles[gaggle]
	if g == nil {
		return apicontract.InteractiveCapabilities{}, ErrDenied
	}
	view, operate := membership(p, g.Spec.InteractiveAccess)
	if g.Spec.InteractiveAccess != nil && !view {
		return apicontract.InteractiveCapabilities{}, ErrDenied
	}
	result := apicontract.InteractiveCapabilities{Gaggle: gaggle, PolicyConfigured: g.Spec.InteractiveAccess != nil, Viewer: view, Operator: operate, SourceWriteMode: "pull-request", Actions: make([]apicontract.InteractiveActionPermission, 0, len(actions))}
	for _, action := range actions {
		result.Actions = append(result.Actions, s.permission(p, g, action))
	}
	return result, nil
}

func (s *Service) permission(p httpapi.Principal, g *apiv1.Gaggle, action apiv1.InteractiveAction) apicontract.InteractiveActionPermission {
	result := apicontract.InteractiveActionPermission{Action: string(action)}
	if g.Spec.InteractiveAccess == nil {
		result.ReasonCode = "policy_missing"
		return result
	}
	if authorize(p, g, action) != nil {
		result.ReasonCode = "action_not_authorized"
		return result
	}
	result.Authorized = true
	result.CredentialConfigured = !providerAction(action) || s.hasCredential(g, action)
	if !result.CredentialConfigured {
		result.ReasonCode = "credential_not_configured"
		return result
	}
	if action == "run.intervene" || (action == "run.restartStage" && s.stageRestartAvailable.Load()) {
		result.Available = true
		return result
	}
	// This permission surface does not itself implement any other effect or session.
	// Concrete routes can advertise availability once they use this authority.
	result.ReasonCode = "operation_not_implemented"
	return result
}

func (s *Service) hasCredential(g *apiv1.Gaggle, action apiv1.InteractiveAction) bool {
	if actionTarget(action, Target{Kind: "backlog"}) {
		_, err := selectSource(g, s.sources, Target{Kind: "backlog"})
		return err == nil
	}
	for _, binding := range g.Spec.InteractiveAccess.Credentials.Repositories {
		if _, err := selectSource(g, s.sources, Target{Kind: "repository", Repository: binding.Repository}); err == nil {
			return true
		}
	}
	return false
}

// SetStageRestartAvailable advertises the daemon's installed interactive
// execution adapter. It defaults off and does not grant human permission.
func (s *Service) SetStageRestartAvailable(available bool) { s.stageRestartAvailable.Store(available) }
