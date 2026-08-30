package executor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/boundedwait"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/providers"
)

// InputKind is the env.Inputs key that selects which deterministic stage kind
// to run. Its absence means KindShell — the common case.
const InputKind = boundedwait.InputKind

// KindShell and KindCIPoll are the built-in deterministic-stage kinds.
// KindShell is the implicit default.
const (
	KindShell  = boundedwait.KindShell
	KindCIPoll = boundedwait.KindCIPoll
)

// KindExecutor runs one registered deterministic-stage kind.
type KindExecutor interface {
	Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error)
}

// KindRegistry maps deterministic-stage kind names to their executors.
type KindRegistry struct {
	executors map[string]KindExecutor
}

// NewKindRegistry returns an empty deterministic-stage kind registry.
func NewKindRegistry() *KindRegistry {
	return &KindRegistry{executors: make(map[string]KindExecutor)}
}

// Register adds an executor under kind. Registrations must be complete before
// the registry is passed to NewTaskExecutor.
func (r *KindRegistry) Register(kind string, executor KindExecutor) error {
	if r == nil {
		return errors.New("executor: kind registry must not be nil")
	}
	if kind == "" {
		return errors.New("executor: kind must not be empty")
	}
	if isNilKindExecutor(executor) {
		return fmt.Errorf("executor: kind %q executor must not be nil", kind)
	}
	if r.executors == nil {
		r.executors = make(map[string]KindExecutor)
	}
	if _, exists := r.executors[kind]; exists {
		return fmt.Errorf("executor: kind %q already registered", kind)
	}
	r.executors[kind] = executor
	return nil
}

// TaskExecutor implements invoke.Deterministic and is the single dispatcher a
// caller registers for apiv1.TaskDeterministic: the runner constructs one
// invoke.Deterministic per run (internal/runner's NewDeterministic factory), so
// every deterministic-stage kind has to be reachable through this registry,
// selected by env.Inputs[InputKind].
type TaskExecutor struct {
	executors map[string]KindExecutor
}

// NewTaskExecutor returns a dispatcher over the registered kind executors.
// KindShell is required because it is the implicit default.
func NewTaskExecutor(registry *KindRegistry) (*TaskExecutor, error) {
	if registry == nil {
		return nil, errors.New("executor: kind registry must not be nil")
	}
	if shell, ok := registry.executors[KindShell]; !ok || isNilKindExecutor(shell) {
		return nil, errors.New("executor: shell executor must not be nil")
	}
	executors := make(map[string]KindExecutor, len(registry.executors))
	for kind, executor := range registry.executors {
		if isNilKindExecutor(executor) {
			return nil, fmt.Errorf("executor: kind %q executor must not be nil", kind)
		}
		executors[kind] = executor
	}
	return &TaskExecutor{executors: executors}, nil
}

func isNilKindExecutor(executor KindExecutor) bool {
	if executor == nil {
		return true
	}
	value := reflect.ValueOf(executor)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Run implements invoke.Deterministic, dispatching to the registered executor
// selected by env.Inputs[InputKind].
func (t *TaskExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	kind := stringInput(env, InputKind)
	if kind == "" {
		kind = KindShell
	}
	executor, ok := t.executors[kind]
	if !ok {
		return apiv1.ResultEnvelope{}, errors.New("executor: unknown " + InputKind + " " + kind)
	}
	return executor.Run(ctx, env, run)
}

type ciPollKindExecutor struct {
	executor *CIPollExecutor
}

// NewCIPollKindExecutor adapts a CIPollExecutor to KindExecutor. A nil
// CIPollExecutor is valid so installations without a PR provider retain the
// existing fail-closed error when kind=ci-poll is requested.
func NewCIPollKindExecutor(executor *CIPollExecutor) KindExecutor {
	return &ciPollKindExecutor{executor: executor}
}

func (e *ciPollKindExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	required := string(capability.ProviderPRWrite)
	if !containsString(env.Capabilities, required) {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: kind=%s requires declared capability %q", KindCIPoll, required)
	}
	if e.executor == nil {
		return apiv1.ResultEnvelope{}, errors.New("executor: kind=ci-poll declared but no CIPollExecutor is configured")
	}
	cfg, err := CIPollConfigFromEnvelope(env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	result, err := e.executor.Run(ctx, cfg)
	if err == nil {
		return result, nil
	}
	if providers.IsTransientError(err) {
		return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(StageFailure(CIPollFailureCode(err), err))
	}
	var providerErr *ciPollProviderError
	if errors.As(err, &providerErr) {
		return apiv1.ResultEnvelope{
			Status:  apiv1.ResultFailure,
			Summary: "ci-poll provider request failed",
			Error: &apiv1.ErrorInfo{
				Code:    CIPollProviderErrorCode,
				Message: err.Error(),
			},
		}, nil
	}
	return result, err
}

// CIPollProviderErrorCode is the ci-poll failure code for a provider request
// that failed for a reason other than an exhausted quota — a terminal 4xx, a
// malformed response, or an exhausted transient-retry budget. Exported
// because the POD path (cmd/goobers/dispatchcipoll.go) must name a ci-poll
// failure exactly as the in-process path does; a second spelling on the pod
// side is a stage that journals under a different code depending on where it
// ran, which is the divergence class the parity work exists to close.
const CIPollProviderErrorCode = "poll_provider_error"

// OutputRateLimitReset is the ResultEnvelope.Outputs key carrying, as an
// RFC3339 timestamp, when the provider says its quota window rolls over. It
// is read by internal/runner's outputRateLimitReset (which drives
// Config.RateLimited -> ProviderQuotaState.RecordExhausted, #712) and by
// shell.go's rate-limit-aware infrastructure backoff.
//
// IT IS THE WHOLE MECHANISM BY WHICH A POD-EXECUTED ci-poll REPORTS QUOTA.
// On a self runner the ci-poll provider is constructed WITH a
// providers.QuotaObserver (cmd/goobers/runnerwiring_executors.go's
// buildCIPollExecutor), so the daemon's ProviderQuotaState is consulted
// BEFORE each request and updated from every response header. A pod has no
// such observer — the quota state lives in the daemon process — so the only
// channel left is the surrendered result, and it is a report AFTER the fact.
// See cmd/goobers/dispatchcipoll.go for the named, accepted degradation this
// implies (decision 005 C5 / finding 002).
const OutputRateLimitReset = "rateLimitReset"

// CIPollFailureCode names a ci-poll provider failure so every consumer — the
// runner's journal, the pod's surrendered envelope, and the daemon's
// RateLimited observer — reads the SAME code for the same cause. A
// rate-limited poll and a flaky 5xx are the same "retry later" decision but
// different operator problems, and both used to reach the journal as the
// generic executor_error.
func CIPollFailureCode(err error) string {
	var rateLimited *providers.RateLimitError
	if errors.As(err, &rateLimited) {
		return providers.ErrorCodeRateLimited
	}
	return CIPollProviderErrorCode
}

// CIPollRateLimitReset returns the provider's quota reset time when err is a
// rate-limit rejection that carries one, formatted as the RFC3339 string
// OutputRateLimitReset takes. A rate limit with no reset header yields
// ("", false): the consumers all treat a missing reset as "nothing
// actionable" (internal/runner's taskOutcome skips notifyRateLimited
// entirely), so an empty or zero-valued key would be strictly worse than an
// absent one.
func CIPollRateLimitReset(err error) (string, bool) {
	var rateLimited *providers.RateLimitError
	if !errors.As(err, &rateLimited) || rateLimited.Reset.IsZero() {
		return "", false
	}
	return rateLimited.Reset.UTC().Format(time.RFC3339), true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
