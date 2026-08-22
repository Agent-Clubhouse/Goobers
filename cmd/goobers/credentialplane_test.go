package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// credentialplane_test.go proves the credential plane's daemon service
// (distributed-state-and-coordination.md §11, DS9/DS10, §13 items 7/8's
// service-side halves): stage-scoped capability gating against the run's
// PINNED definition, goober-scoped injector construction through the same
// machinery buildCredentialEnv resolves through, scrubber registration before
// the response, TTL threading, and the values-free audit trail.

const credentialPlaneTestSecret = "sekret-minted-value-0123456789abcdef"

// credentialPlaneSpec is an implement→review workflow: an agentic task with
// declared credential capabilities, a deterministic follow-up, and an agentic
// reviewer gate.
func credentialPlaneSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskAgentic, Goober: "dev", Goal: "implement",
				Capabilities: []string{"repo:push", "github:issues:write"},
				Next:         "push-branch",
			},
			{
				Name: "push-branch", Type: apiv1.TaskDeterministic, Goal: "publish the branch",
				Capabilities: []string{"repo:push"},
				Run:          &apiv1.DeterministicRun{Command: []string{"true"}},
				Next:         "review",
			},
		},
		Gates: []apiv1.Gate{{
			Name:      "review",
			Evaluator: apiv1.EvaluatorAgentic,
			Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
			Branches: map[string]string{
				"pass":          workflow.TerminalComplete,
				"fail":          workflow.TargetAbort,
				"needs-changes": "implement",
			},
		}},
	}
}

func compileCredentialPlaneMachine(t *testing.T, spec apiv1.WorkflowSpec) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(
		workflow.Definition{Name: "implementation", Version: 1, Spec: spec},
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile fixture workflow: %v", err)
	}
	return machine
}

// writePinnedRun scaffolds a run journal whose pinned workflow-definition
// snapshot reconstructs machine — the WF-016 pin the plane verifies stages
// against.
func writePinnedRun(t *testing.T, layout instance.Layout, gaggle, runID string, machine *workflow.Machine) {
	t.Helper()
	definition, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(
		layout.ForGaggle(gaggle).RunsDir(),
		journal.RunIdentity{
			RunID:           runID,
			Workflow:        machine.Def.Name,
			WorkflowVersion: machine.Def.Version,
			WorkflowDigest:  machine.Digest(),
			Gaggle:          gaggle,
			Trigger:         journal.Trigger{Kind: journal.TriggerManual},
		},
		map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
	)
	if err != nil {
		t.Fatalf("create pinned run fixture: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func credentialPlaneGoobers() map[string]apiv1.GooberSpec {
	return map[string]apiv1.GooberSpec{
		"dev":      {Gaggle: "web", Capabilities: []string{"repo:push", "github:issues:write"}},
		"reviewer": {Gaggle: "web", Capabilities: []string{"github:issues:write"}},
	}
}

// newCredentialPlaneFixture wires a service over a temp instance: a shared
// exact-value registry, an instance log scrubbed through it (exactly the
// daemon's own wiring), a pinned run, and a fake credential source with one
// expiring grant and one static grant.
func newCredentialPlaneFixture(t *testing.T, machine *workflow.Machine) (*daemonCredentialService, *journal.RegistryScrubber, string) {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	shared, chain := journal.DefaultScrubber()
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithScrubber(chain))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	const runID = "run-credential-plane"
	writePinnedRun(t, layout, "web", runID, machine)

	expires := time.Now().Add(50 * time.Minute).UTC().Truncate(time.Second)
	service := newDaemonCredentialService(layout, &instance.Config{}, nil, shared, log)
	service.Replace(credentialPlaneDefinitions{
		Scopes:  map[string]credentialGaggleScope{"web": {Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}}},
		Goobers: credentialPlaneGoobers(),
	})
	service.buildSources = func(credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {
		resolver, err := credentials.NewResolverWithExpiring(nil, nil,
			map[string]credentials.ResolveFunc{
				"issues-ref": func(context.Context) (string, error) { return "static-issues-token-9876543210", nil },
			},
			map[string]credentials.ExpiringResolveFunc{
				"repo-ref": func(context.Context) (string, time.Time, error) {
					return credentialPlaneTestSecret, expires, nil
				},
			},
		)
		if err != nil {
			return nil, nil, err
		}
		return resolver, []credentials.Grant{
			{Capability: "repo:push", Ref: "repo-ref"},
			{Capability: "github:issues:write", Ref: "issues-ref"},
		}, nil
	}
	return service, shared, runID
}

func planeErrorOf(t *testing.T, err error) *httpapi.InterventionError {
	t.Helper()
	var planeErr *httpapi.InterventionError
	if !errors.As(err, &planeErr) {
		t.Fatalf("error = %v (%T), want a typed *httpapi.InterventionError", err, err)
	}
	return planeErr
}

func TestCredentialPlaneResolvesStageScopedCredentials(t *testing.T) {
	machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	service, _, runID := newCredentialPlaneFixture(t, machine)

	response, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "implement",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	byCapability := map[string]httpapi.MintedCredential{}
	for _, credential := range response.Credentials {
		byCapability[credential.Capability] = credential
	}
	if len(byCapability) != 2 {
		t.Fatalf("credentials = %+v, want repo:push and issues:write", response.Credentials)
	}
	repo := byCapability["repo:push"]
	if repo.Value != credentialPlaneTestSecret {
		t.Fatalf("repo:push value = %q", repo.Value)
	}
	// DS10: the mint response carries the credential's expiry when the source
	// states one — and does not invent one when it doesn't.
	if repo.ExpiresAt == nil || !repo.ExpiresAt.After(time.Now()) {
		t.Fatalf("repo:push expiresAt = %v, want the source's stated future expiry", repo.ExpiresAt)
	}
	if issues := byCapability["github:issues:write"]; issues.ExpiresAt != nil {
		t.Fatalf("issues:write expiresAt = %v, want none (static source)", issues.ExpiresAt)
	}

	// A subset re-resolve (the one-retry-on-401 path) returns fresh values
	// for exactly the named capability.
	retry, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "implement", Capabilities: []string{"repo:push"},
	})
	if err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if len(retry.Credentials) != 1 || retry.Credentials[0].Capability != "repo:push" {
		t.Fatalf("re-resolve credentials = %+v", retry.Credentials)
	}
}

// TestCredentialPlaneRefusesUndeclaredCapability pins the typed 403: nothing
// materializes for a capability the stage did not declare, and the deny names
// the capability (§13 item 7's service half).
func TestCredentialPlaneRefusesUndeclaredCapability(t *testing.T) {
	machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	service, shared, runID := newCredentialPlaneFixture(t, machine)

	_, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "implement", Capabilities: []string{"provider:admin"},
	})
	planeErr := planeErrorOf(t, err)
	if planeErr.Status != http.StatusForbidden || planeErr.Code != "capability_undeclared" {
		t.Fatalf("refusal = %d %s, want 403 capability_undeclared", planeErr.Status, planeErr.Code)
	}
	if !strings.Contains(planeErr.Message, `"provider:admin"`) || !strings.Contains(planeErr.Message, `"implement"`) {
		t.Fatalf("refusal message %q must name the capability and the stage", planeErr.Message)
	}
	// The deny happened before any resolution: nothing was minted, so nothing
	// was registered.
	if got := shared.Scrub([]byte(credentialPlaneTestSecret)); string(got) != credentialPlaneTestSecret {
		t.Fatal("a refused resolve minted a value")
	}

	// The deterministic stage declares only repo:push; github:issues:write is
	// declared elsewhere in the workflow but not by THIS stage.
	_, err = service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "push-branch", Capabilities: []string{"github:issues:write"},
	})
	planeErr = planeErrorOf(t, err)
	if planeErr.Status != http.StatusForbidden || planeErr.Code != "capability_undeclared" {
		t.Fatalf("cross-stage refusal = %d %s, want 403 capability_undeclared", planeErr.Status, planeErr.Code)
	}
}

// TestCredentialPlaneEmptyDeclarationResolvesNothing pins §13 item 7's second
// half verbatim: "a stage whose declared capabilities are empty can resolve
// nothing" — the full-set resolve answers with zero credentials (and mints
// none), and naming any capability is the typed 403.
func TestCredentialPlaneEmptyDeclarationResolvesNothing(t *testing.T) {
	spec := credentialPlaneSpec()
	spec.Tasks[1].Capabilities = nil // push-branch declares nothing
	machine := compileCredentialPlaneMachine(t, spec)
	service, shared, runID := newCredentialPlaneFixture(t, machine)

	response, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "push-branch",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(response.Credentials) != 0 {
		t.Fatalf("credentials = %+v, want none for a stage declaring no capabilities", response.Credentials)
	}
	if got := shared.Scrub([]byte(credentialPlaneTestSecret)); string(got) != credentialPlaneTestSecret {
		t.Fatal("an empty-declaration resolve minted a value")
	}

	_, err = service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "push-branch", Capabilities: []string{"repo:push"},
	})
	planeErr := planeErrorOf(t, err)
	if planeErr.Status != http.StatusForbidden || planeErr.Code != "capability_undeclared" {
		t.Fatalf("refusal = %d %s, want 403 capability_undeclared", planeErr.Status, planeErr.Code)
	}
}

func TestCredentialPlaneRefusesUnknownStageAndRun(t *testing.T) {
	machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	service, _, runID := newCredentialPlaneFixture(t, machine)

	_, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "exfiltrate",
	})
	planeErr := planeErrorOf(t, err)
	if planeErr.Status != http.StatusNotFound || planeErr.Code != "stage_unknown" {
		t.Fatalf("unknown stage refusal = %d %s, want 404 stage_unknown", planeErr.Status, planeErr.Code)
	}

	_, err = service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: "run-that-never-existed", Stage: "implement",
	})
	planeErr = planeErrorOf(t, err)
	if planeErr.Status != http.StatusNotFound || planeErr.Code != "run_not_found" {
		t.Fatalf("unknown run refusal = %d %s, want 404 run_not_found", planeErr.Status, planeErr.Code)
	}
}

// TestCredentialPlaneVerifiesAgainstThePinnedDefinition proves the stage
// identity is checked against the run's PINNED definition, not the currently
// served one: a run pinned to a definition where implement declares only
// issues:write refuses repo:push even though the current config's workflow
// (and the goober) would grant it — and a run whose pin cannot be verified
// resolves nothing at all.
func TestCredentialPlaneVerifiesAgainstThePinnedDefinition(t *testing.T) {
	narrowSpec := credentialPlaneSpec()
	narrowSpec.Tasks[0].Capabilities = []string{"github:issues:write"}
	narrow := compileCredentialPlaneMachine(t, narrowSpec)
	service, _, runID := newCredentialPlaneFixture(t, narrow)

	_, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "implement", Capabilities: []string{"repo:push"},
	})
	planeErr := planeErrorOf(t, err)
	if planeErr.Status != http.StatusForbidden || planeErr.Code != "capability_undeclared" {
		t.Fatalf("pinned-narrower refusal = %d %s, want 403 capability_undeclared", planeErr.Status, planeErr.Code)
	}

	// A run whose pinned snapshot fails digest verification fails closed.
	broken := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	brokenService, _, _ := newCredentialPlaneFixture(t, broken)
	brokenService.pinnedMachine = func(*journal.Reader, journal.RunIdentity) (*workflow.Machine, error) {
		return nil, errors.New("digest mismatch")
	}
	_, err = brokenService.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: "run-credential-plane", Stage: "implement",
	})
	planeErr = planeErrorOf(t, err)
	if planeErr.Status != http.StatusConflict || planeErr.Code != "run_pin_unverifiable" {
		t.Fatalf("broken-pin refusal = %d %s, want 409 run_pin_unverifiable", planeErr.Status, planeErr.Code)
	}
}

// TestCredentialPlaneResolvesGateReviewerScope: an agentic reviewer gate
// resolves the reviewer goober's own declared capabilities (#294) — and only
// those: the gate's reviewer never receives the implement stage's broader
// repo:push grant.
func TestCredentialPlaneResolvesGateReviewerScope(t *testing.T) {
	machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	service, _, runID := newCredentialPlaneFixture(t, machine)

	response, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "review",
	})
	if err != nil {
		t.Fatalf("Resolve gate: %v", err)
	}
	if len(response.Credentials) != 1 || response.Credentials[0].Capability != "github:issues:write" {
		t.Fatalf("gate credentials = %+v, want exactly issues:write", response.Credentials)
	}

	_, err = service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "review", Capabilities: []string{"repo:push"},
	})
	planeErr := planeErrorOf(t, err)
	if planeErr.Status != http.StatusForbidden || planeErr.Code != "capability_undeclared" {
		t.Fatalf("reviewer repo:push refusal = %d %s, want 403 capability_undeclared", planeErr.Status, planeErr.Code)
	}
}

// TestCredentialPlaneRegistersMintedValuesWithTheScrubber is requirement (3)
// of the plane, proven at rest: every minted value registers with the shared
// exact-value registry BEFORE the response is written, so a value that later
// leaks into a journal line lands redacted — asserted against the actual
// bytes on disk, not the in-memory event.
func TestCredentialPlaneRegistersMintedValuesWithTheScrubber(t *testing.T) {
	machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	service, shared, runID := newCredentialPlaneFixture(t, machine)

	if _, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "implement",
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// The registration is observable immediately after the call returns.
	if got := shared.Scrub([]byte("x " + credentialPlaneTestSecret + " y")); strings.Contains(string(got), credentialPlaneTestSecret) {
		t.Fatal("a minted value is not registered with the shared scrubber registry")
	}

	// Journal a line that leaks the minted value through the instance log the
	// service itself writes to (scrubbed via the same shared registry).
	if err := service.log.Append(journal.Event{
		Type: journal.EventRunnerAnnotation,
		Runner: map[string]any{
			"kind": "test.leak",
			"note": "oops: " + credentialPlaneTestSecret,
		},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(service.layout.SchedulerDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), credentialPlaneTestSecret) {
		t.Fatal("a minted value survived into the instance journal at rest")
	}
	if !strings.Contains(string(data), journal.Redacted) {
		t.Fatal("the leaked line was dropped rather than redacted")
	}
}

// TestCredentialPlaneAuditTrailNamesCapabilitiesNeverValues is requirement
// (5)'s audit half: the resolution's instance-log event records WHICH
// capabilities were resolved for WHICH stage — and no credential value.
func TestCredentialPlaneAuditTrailNamesCapabilitiesNeverValues(t *testing.T) {
	machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	service, _, runID := newCredentialPlaneFixture(t, machine)

	if _, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "implement",
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(service.layout.SchedulerDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var audit *journal.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event journal.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("parse instance event %q: %v", line, err)
		}
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == credentialResolutionMarker {
			audit = &event
			break
		}
	}
	if audit == nil {
		t.Fatal("no credentials.resolved audit event was journaled")
	}
	if audit.RunID != runID || audit.Stage != "implement" || audit.Gaggle != "web" {
		t.Fatalf("audit event = %+v, want run/stage/gaggle identity", audit)
	}
	materialized, _ := audit.Runner["materialized"].([]any)
	names := make([]string, 0, len(materialized))
	for _, name := range materialized {
		names = append(names, name.(string))
	}
	if len(names) != 2 || !strings.Contains(strings.Join(names, ","), "repo:push") {
		t.Fatalf("audit materialized = %v, want the capability names", names)
	}
	if strings.Contains(string(data), credentialPlaneTestSecret) {
		t.Fatal("the audit trail contains a credential value")
	}
}

// TestCredentialPlaneHoldsNoValuesAcrossRequests is requirement (5)'s
// no-cache half at the service seam: every request rebuilds its sources, so
// consecutive resolves observe fresh values from the underlying source, never
// a plane-held copy.
func TestCredentialPlaneHoldsNoValuesAcrossRequests(t *testing.T) {
	machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	service, _, runID := newCredentialPlaneFixture(t, machine)

	mints := 0
	service.buildSources = func(credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {
		resolver, err := credentials.NewResolverWithExpiring(nil, nil, map[string]credentials.ResolveFunc{
			"repo-ref": func(context.Context) (string, error) {
				mints++
				return credentialPlaneTestSecret, nil
			},
		}, nil)
		if err != nil {
			return nil, nil, err
		}
		return resolver, []credentials.Grant{{Capability: "repo:push", Ref: "repo-ref"}}, nil
	}
	for i := 0; i < 2; i++ {
		if _, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
			RunID: runID, Stage: "implement", Capabilities: []string{"repo:push"},
		}); err != nil {
			t.Fatalf("Resolve %d: %v", i, err)
		}
	}
	if mints != 2 {
		t.Fatalf("source resolved %d times for 2 requests; the plane must not cache values across requests", mints)
	}
}
