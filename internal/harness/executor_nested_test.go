package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func nestedEnvelope(workspace string) apiv1.InvocationEnvelope {
	env := testEnvelope(workspace, "repo:read", "repo:push")
	env.Attempt = 2
	env.Goober = "coder"
	env.OwnershipBoundary = "task:implement"
	env.PolicyActions = []string{"modify-repository", "approve-release"}
	env.Limits = apiv1.Limits{MaxDurationSeconds: 120, MaxTokens: 2000, MaxCostUSD: 20}
	env.Item = &apiv1.BacklogItem{ID: "3254", Provider: apiv1.ProviderGitHub, Title: "nested agents"}
	env.Inputs = map[string]any{"parent-only": "context"}
	env.InstructionAddendum = "parent orchestration note"
	env.AdditionalWorkspaces = []apiv1.AdditionalWorkspace{
		{Name: "docs", Path: workspace + "-docs"},
	}
	parent := apiv1.PlatformPolicy{
		Capabilities:       append([]string(nil), env.Capabilities...),
		PolicyActions:      append([]string(nil), env.PolicyActions...),
		Credentials:        []string{"repo:read", "repo:push"},
		Sandbox:            "workspace",
		FilesystemRoots:    []string{"workspace", "workspace:docs"},
		NetworkEgress:      []string{"github", "packages"},
		ContentExclusions:  []string{"secrets"},
		Budget:             env.Limits,
		Cancellation:       "stage-context",
		CompletionContract: "result",
	}
	env.ParentPlatformPolicy = &parent
	env.NestedAgentPolicy = &apiv1.NestedAgentPolicy{
		Version:           apiv1.NestedAgentPolicyVersion,
		Delegation:        apiv1.DelegationDisabled,
		PermittedProfiles: []string{"worker"},
		Context:           apiv1.NestedContextPolicy{Mode: apiv1.ContextInherited},
		PlatformPolicy: apiv1.PlatformPolicy{
			Capabilities:       []string{"repo:read"},
			PolicyActions:      []string{"modify-repository"},
			Credentials:        []string{"repo:read"},
			Sandbox:            "workspace",
			FilesystemRoots:    []string{"workspace"},
			NetworkEgress:      []string{"github"},
			ContentExclusions:  []string{"generated"},
			Budget:             apiv1.Limits{MaxDurationSeconds: 30, MaxTokens: 500, MaxCostUSD: 5},
			Cancellation:       "stage-context",
			CompletionContract: "result",
		},
	}
	return env
}

func nestedTestInjector(t *testing.T) *credentials.Injector {
	t.Helper()
	t.Setenv("NESTED_REPO_READ_TOKEN", "nested-read-token")
	t.Setenv("NESTED_REPO_PUSH_TOKEN", "nested-push-token")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "nested-read", Env: "NESTED_REPO_READ_TOKEN"},
		{Name: "nested-push", Env: "NESTED_REPO_PUSH_TOKEN"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{
		{Capability: "repo:read", Ref: "nested-read"},
		{Capability: "repo:push", Ref: "nested-push"},
	}, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	return injector
}

func nestedExecutor(t *testing.T, adapter Adapter, opts ...Option) *Executor {
	t.Helper()
	recorder := &fakeRecorder{}
	executor, err := NewExecutor(
		adapter,
		nestedTestInjector(t),
		recorder,
		recorder,
		recorder,
		journal.NewPatternScrubber(),
		"delegate freely to any child agent",
		opts...,
	)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	return executor
}

func TestNestedPolicyCannotUseOrdinaryOrUnsupportedProductionAdapterPaths(t *testing.T) {
	env := nestedEnvelope(t.TempDir())
	req := RunRequest{Envelope: env}
	if _, err := (&FakeAdapter{}).Run(context.Background(), req); err == nil ||
		!strings.Contains(err.Error(), "requires the adapter child-launch path") {
		t.Fatalf("FakeAdapter.Run error = %v, want nested path refusal", err)
	}
	if _, ok := any(&ClaudeAdapter{}).(NestedPolicyCapability); ok {
		t.Fatal("ClaudeAdapter advertises nested policy support without a policy-enforcing child launcher")
	}
	if _, ok := any(&CopilotAdapter{}).(NestedPolicyCapability); ok {
		t.Fatal("CopilotAdapter advertises nested policy support without a policy-enforcing child launcher")
	}
}

func TestExecutorNestedRunUsesEffectiveChildLaunchPath(t *testing.T) {
	ordinaryCalls := 0
	nestedCalls := 0
	adapter := &FakeAdapter{
		Act: func(context.Context, RunRequest) error {
			ordinaryCalls++
			return errors.New("ordinary path must not run")
		},
		NestedAct: func(ctx context.Context, req RunRequest) error {
			nestedCalls++
			if req.ExecutionPolicy == nil {
				return errors.New("missing execution policy")
			}
			if req.ExecutionPolicy.Delegation != apiv1.DelegationDisabled {
				return fmt.Errorf("delegation = %q, want disabled", req.ExecutionPolicy.Delegation)
			}
			if !slices.Equal(req.Envelope.Capabilities, []string{"repo:read"}) ||
				!slices.Equal(req.Envelope.PolicyActions, []string{"modify-repository"}) {
				return fmt.Errorf("runtime authority = (%v, %v)", req.Envelope.Capabilities, req.Envelope.PolicyActions)
			}
			if req.Envelope.ParentPlatformPolicy != nil {
				return errors.New("parent authority leaked to child")
			}
			if len(req.Envelope.AdditionalWorkspaces) != 0 {
				return fmt.Errorf("additional workspaces = %v, want none", req.Envelope.AdditionalWorkspaces)
			}
			if req.Timeout != 30*time.Second {
				return fmt.Errorf("timeout = %v, want 30s", req.Timeout)
			}
			token, err := req.Credentials.Token(ctx, "repo:read")
			if err != nil {
				return fmt.Errorf("repo:read credential: %w", err)
			}
			if token != "nested-read-token" {
				return fmt.Errorf("repo:read token = %q, want nested-read-token", token)
			}
			if _, err := req.Credentials.Token(ctx, "repo:push"); err == nil {
				return errors.New("repo:push credential was unexpectedly granted")
			} else if !errors.Is(err, credentials.ErrUndeclaredCapability) {
				return fmt.Errorf("repo:push credential: %w", err)
			}
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}

	result, err := nestedExecutor(t, adapter).Invoke(context.Background(), nestedEnvelope(t.TempDir()))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultSuccess || nestedCalls != 1 || ordinaryCalls != 0 {
		t.Fatalf("result=%q nestedCalls=%d ordinaryCalls=%d", result.Status, nestedCalls, ordinaryCalls)
	}
}

type standardOnlyAdapter struct {
	called bool
}

func (a *standardOnlyAdapter) Name() string { return "standard-only" }
func (a *standardOnlyAdapter) Preflight(context.Context) (PreflightInfo, error) {
	return PreflightInfo{Version: "test"}, nil
}
func (a *standardOnlyAdapter) Run(context.Context, RunRequest) (Outcome, error) {
	a.called = true
	return Outcome{}, errors.New("standard adapter was called")
}

func TestExecutorNestedAdmissionFailsClosedWithoutAuthorityOrAdapterSupport(t *testing.T) {
	t.Run("missing parent authority", func(t *testing.T) {
		called := false
		adapter := &FakeAdapter{NestedAct: func(context.Context, RunRequest) error {
			called = true
			return nil
		}}
		env := nestedEnvelope(t.TempDir())
		env.ParentPlatformPolicy = nil
		if _, err := nestedExecutor(t, adapter).Invoke(context.Background(), env); err == nil ||
			!strings.Contains(err.Error(), "parent platform authority is required") {
			t.Fatalf("Invoke error = %v, want missing parent authority", err)
		}
		if called {
			t.Fatal("nested adapter ran before parent authority admission")
		}
	})

	t.Run("unsupported adapter", func(t *testing.T) {
		adapter := &standardOnlyAdapter{}
		if _, err := nestedExecutor(t, adapter).Invoke(context.Background(), nestedEnvelope(t.TempDir())); err == nil ||
			!strings.Contains(err.Error(), "does not support nested-agent policy enforcement") {
			t.Fatalf("Invoke error = %v, want unsupported adapter", err)
		}
		if adapter.called {
			t.Fatal("ordinary adapter ran for unsupported nested policy")
		}
	})
}

func TestExecutorNestedContextModes(t *testing.T) {
	pointers := []apiv1.ContextPointer{
		{Name: "issue", External: &apiv1.ExternalRef{Kind: "issue", URI: "https://example.test/issues/3254"}},
		{Name: "design", External: &apiv1.ExternalRef{Kind: "url", URI: "https://example.test/design"}},
	}
	tests := []struct {
		name    string
		context apiv1.NestedContextPolicy
		check   func(RunRequest) error
	}{
		{
			name:    "inherited",
			context: apiv1.NestedContextPolicy{Mode: apiv1.ContextInherited},
			check: func(req RunRequest) error {
				if len(req.Envelope.ContextPointers) != 2 || req.Envelope.Item == nil ||
					req.Envelope.Inputs["parent-only"] != "context" ||
					req.Envelope.InstructionAddendum == "" {
					return fmt.Errorf("inherited context was filtered: %+v", req.Envelope)
				}
				return nil
			},
		},
		{
			name:    "fresh",
			context: apiv1.NestedContextPolicy{Mode: apiv1.ContextFresh},
			check: func(req RunRequest) error {
				if len(req.Envelope.ContextPointers) != 0 || req.Envelope.Item != nil ||
					len(req.Envelope.Inputs) != 0 || req.Envelope.InstructionAddendum != "" {
					return fmt.Errorf("fresh context retained optional parent context: %+v", req.Envelope)
				}
				if req.ExecutionPolicy == nil || req.ExecutionPolicy.PlatformPolicy.Sandbox == "" {
					return errors.New("fresh context lost mandatory execution policy")
				}
				return nil
			},
		},
		{
			name: "explicit",
			context: apiv1.NestedContextPolicy{
				Mode:             apiv1.ContextExplicit,
				ArtifactNames:    []string{"design"},
				EnvelopeSections: []string{"objective", "budget"},
			},
			check: func(req RunRequest) error {
				if len(req.Envelope.ContextPointers) != 1 || req.Envelope.ContextPointers[0].Name != "design" ||
					req.Envelope.Item != nil || len(req.Envelope.Inputs) != 0 {
					return fmt.Errorf("explicit context = %+v", req.Envelope)
				}
				if len(req.SelectedEnvelopeSections) != 2 ||
					req.SelectedEnvelopeSections["objective"] != "implement the thing" {
					return fmt.Errorf("selected sections = %#v", req.SelectedEnvelopeSections)
				}
				if _, ok := req.SelectedEnvelopeSections["capabilities"]; ok {
					return fmt.Errorf("unselected capabilities leaked into selected sections")
				}
				return nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := &FakeAdapter{NestedAct: func(_ context.Context, req RunRequest) error {
				if err := test.check(req); err != nil {
					return err
				}
				return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			}}
			env := nestedEnvelope(t.TempDir())
			env.ContextPointers = pointers
			env.NestedAgentPolicy.Context = test.context
			if _, err := nestedExecutor(t, adapter).Invoke(context.Background(), env); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
		})
	}
}

func TestExecutorNestedExplicitContextRejectsUnavailableArtifact(t *testing.T) {
	called := false
	adapter := &FakeAdapter{NestedAct: func(context.Context, RunRequest) error {
		called = true
		return nil
	}}
	env := nestedEnvelope(t.TempDir())
	env.NestedAgentPolicy.Context = apiv1.NestedContextPolicy{
		Mode:          apiv1.ContextExplicit,
		ArtifactNames: []string{"missing"},
	}
	if _, err := nestedExecutor(t, adapter).Invoke(context.Background(), env); err == nil ||
		!strings.Contains(err.Error(), `selected artifact "missing" is unavailable`) {
		t.Fatalf("Invoke error = %v, want unavailable artifact", err)
	}
	if called {
		t.Fatal("nested adapter ran with unavailable explicit context")
	}
}

func TestExecutorNestedRejectsRuntimeWidening(t *testing.T) {
	tests := []struct {
		name string
		edit func(*apiv1.InvocationEnvelope)
	}{
		{"capability", func(env *apiv1.InvocationEnvelope) {
			env.NestedAgentPolicy.PlatformPolicy.Capabilities = []string{"repo:read", "admin"}
		}},
		{"credential", func(env *apiv1.InvocationEnvelope) {
			env.NestedAgentPolicy.PlatformPolicy.Credentials = []string{"repo:read", "admin-token"}
		}},
		{"filesystem", func(env *apiv1.InvocationEnvelope) {
			env.NestedAgentPolicy.PlatformPolicy.FilesystemRoots = []string{"workspace", "host-root"}
		}},
		{"egress", func(env *apiv1.InvocationEnvelope) {
			env.NestedAgentPolicy.PlatformPolicy.NetworkEgress = []string{"internet"}
		}},
		{"sandbox", func(env *apiv1.InvocationEnvelope) {
			env.NestedAgentPolicy.PlatformPolicy.Sandbox = "host"
		}},
		{"budget", func(env *apiv1.InvocationEnvelope) {
			env.NestedAgentPolicy.PlatformPolicy.Budget.MaxDurationSeconds = 121
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			adapter := &FakeAdapter{NestedAct: func(context.Context, RunRequest) error {
				called = true
				return nil
			}}
			env := nestedEnvelope(t.TempDir())
			test.edit(&env)
			if _, err := nestedExecutor(t, adapter).Invoke(context.Background(), env); err == nil {
				t.Fatal("Invoke succeeded with widened authority")
			}
			if called {
				t.Fatal("nested adapter ran before widening was rejected")
			}
		})
	}
}

func TestExecutorNestedRejectsConfiguredReasoningAboveCeiling(t *testing.T) {
	for _, test := range []struct {
		name   string
		effort string
	}{
		{name: "above ceiling", effort: "high"},
		{name: "unsupported adapter-specific level", effort: "xhigh"},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			adapter := &FakeAdapter{NestedAct: func(context.Context, RunRequest) error {
				called = true
				return nil
			}}
			env := nestedEnvelope(t.TempDir())
			env.NestedAgentPolicy.Model.MaxReasoningEffort = apiv1.ReasoningMedium
			options := map[string]apiextensionsv1.JSON{
				"reasoningEffort": {Raw: []byte(`"` + test.effort + `"`)},
			}
			_, err := nestedExecutor(t, adapter, WithHarnessConfig("", options)).Invoke(context.Background(), env)
			if err == nil || !strings.Contains(err.Error(), "reasoning effort") {
				t.Fatalf("Invoke error = %v, want reasoning ceiling refusal", err)
			}
			if called {
				t.Fatal("nested adapter ran above reasoning ceiling")
			}
		})
	}
}
