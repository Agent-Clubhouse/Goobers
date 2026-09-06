package main

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/externaltelemetry"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// credentialplaneexternaltelemetry_test.go is #4341's own regression evidence
// for the credential-plane half: a stage declaring telemetry:read and naming
// a connector in its pinned inputs.connector mints exactly that connector's
// auth secret, resolved from externalTelemetry.connectors rather than any
// credentials: grant — the second #4341 PR (pod dispatch) has no caller for
// this yet, so these tests exercise daemonCredentialService.Resolve directly.

// externalTelemetrySpec is a single deterministic task declaring
// telemetry:read with inputs.connector naming connectorName.
func externalTelemetrySpec(connectorName string) apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@hourly"}},
		Start:    "query-telemetry",
		Tasks: []apiv1.Task{
			{
				Name: "query-telemetry", Type: apiv1.TaskDeterministic, Goal: "query external telemetry",
				Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "external-telemetry"}},
				Inputs:       map[string]string{"kind": "external-telemetry", "connector": connectorName},
				Capabilities: []string{"telemetry:read"},
			},
		},
	}
}

func externalTelemetryConnectorConfig(name string, tokenEnv string) externaltelemetry.ConnectorConfig {
	cfg := externaltelemetry.ConnectorConfig{
		Name: name, Kind: "fake", Version: "v1",
		Config: []byte(`{}`),
	}
	if tokenEnv != "" {
		cfg.Auth = externaltelemetry.AuthConfig{Mode: "static", Token: &externaltelemetry.CredentialRef{Env: tokenEnv}}
	}
	return cfg
}

// newExternalTelemetryCredentialPlaneFixture mirrors newCredentialPlaneFixture
// but wires a config carrying externalTelemetry.connectors instead of
// credentials: grants — the connector secret is resolved from THIS config
// directly, not through buildSources/credentials.Grant.
func newExternalTelemetryCredentialPlaneFixture(t *testing.T, machine *workflow.Machine, connectors ...externaltelemetry.ConnectorConfig) (*daemonCredentialService, string) {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	shared, chain := journal.DefaultScrubber()
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithScrubber(chain))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	const runID = "run-external-telemetry"
	writePinnedRun(t, layout, "web", runID, machine, nil)

	cfg := &instance.Config{ExternalTelemetry: externaltelemetry.Configuration{Connectors: connectors}}
	service := newDaemonCredentialService(layout, cfg, nil, shared, log)
	service.Replace(credentialPlaneDefinitions{
		Scopes: map[string]credentialGaggleScope{"web": {Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}}},
	})
	return service, runID
}

// TestCredentialPlaneMintsExternalTelemetryConnectorSecret is the success
// path: the pinned task's inputs.connector names a real, auth-configured
// connector, and Resolve mints exactly its secret under telemetry:read.
func TestCredentialPlaneMintsExternalTelemetryConnectorSecret(t *testing.T) {
	t.Setenv("FAKE_CONNECTOR_TOKEN", "connector-secret-0123456789abcdef")
	machine := compileCredentialPlaneMachine(t, externalTelemetrySpec("adx-prod"))
	service, runID := newExternalTelemetryCredentialPlaneFixture(t, machine,
		externalTelemetryConnectorConfig("adx-prod", "FAKE_CONNECTOR_TOKEN"))

	response, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "query-telemetry",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(response.Credentials) != 1 || response.Credentials[0].Capability != "telemetry:read" ||
		response.Credentials[0].Value != "connector-secret-0123456789abcdef" {
		t.Fatalf("credentials = %+v, want telemetry:read = the connector's own secret", response.Credentials)
	}
}

// TestCredentialPlaneExternalTelemetryScrubsTheMintedSecret proves the
// resolved value is registered with the shared scrubber before the response
// is built, matching every other minted credential's contract.
func TestCredentialPlaneExternalTelemetryScrubsTheMintedSecret(t *testing.T) {
	t.Setenv("FAKE_CONNECTOR_TOKEN", "connector-secret-to-scrub-abcdef")
	machine := compileCredentialPlaneMachine(t, externalTelemetrySpec("adx-prod"))
	layout := instance.NewLayout(t.TempDir())
	shared, chain := journal.DefaultScrubber()
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithScrubber(chain))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	const runID = "run-external-telemetry-scrub"
	writePinnedRun(t, layout, "web", runID, machine, nil)
	cfg := &instance.Config{ExternalTelemetry: externaltelemetry.Configuration{
		Connectors: []externaltelemetry.ConnectorConfig{externalTelemetryConnectorConfig("adx-prod", "FAKE_CONNECTOR_TOKEN")},
	}}
	service := newDaemonCredentialService(layout, cfg, nil, shared, log)
	service.Replace(credentialPlaneDefinitions{
		Scopes: map[string]credentialGaggleScope{"web": {Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}}},
	})

	if _, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{RunID: runID, Stage: "query-telemetry"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if scrubbed := string(chain.Scrub([]byte("prefix connector-secret-to-scrub-abcdef suffix"))); scrubbed == "prefix connector-secret-to-scrub-abcdef suffix" {
		t.Fatalf("minted connector secret was not registered with the scrubber: %q", scrubbed)
	}
}

// TestCredentialPlaneRefusesUnknownExternalTelemetryConnector is the
// fail-closed path: a pinned inputs.connector naming a connector no longer
// in the served config is refused, not silently skipped.
func TestCredentialPlaneRefusesUnknownExternalTelemetryConnector(t *testing.T) {
	machine := compileCredentialPlaneMachine(t, externalTelemetrySpec("removed-connector"))
	service, runID := newExternalTelemetryCredentialPlaneFixture(t, machine)

	_, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "query-telemetry",
	})
	if err == nil {
		t.Fatal("Resolve succeeded naming a connector absent from config, want a refusal")
	}
	planeErr := planeErrorOf(t, err)
	if planeErr.Code != "connector_unavailable" {
		t.Fatalf("error code = %q, want connector_unavailable", planeErr.Code)
	}
}

// TestCredentialPlaneExternalTelemetryConnectorWithNoAuthMintsNothing covers
// the none/ambient auth modes: a connector with no declared Auth.Token
// resolves telemetry:read to nothing, not an error — the pod's own connector
// construction (the second #4341 PR) then runs with no credential for it,
// exactly as local execution does today.
func TestCredentialPlaneExternalTelemetryConnectorWithNoAuthMintsNothing(t *testing.T) {
	machine := compileCredentialPlaneMachine(t, externalTelemetrySpec("ambient-connector"))
	service, runID := newExternalTelemetryCredentialPlaneFixture(t, machine,
		externalTelemetryConnectorConfig("ambient-connector", ""))

	response, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "query-telemetry",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(response.Credentials) != 0 {
		t.Fatalf("credentials = %+v, want none for a connector with no declared auth token", response.Credentials)
	}
}

// TestCredentialPlaneExternalTelemetryOnlyMintsTheNamedConnector proves
// scoping: with two connectors configured, only the one this stage's pinned
// inputs.connector names is ever resolved — the acceptance criterion that a
// real connector receives only its OWN declared secret material.
func TestCredentialPlaneExternalTelemetryOnlyMintsTheNamedConnector(t *testing.T) {
	t.Setenv("WANTED_CONNECTOR_TOKEN", "wanted-secret-0123456789")
	t.Setenv("OTHER_CONNECTOR_TOKEN", "other-secret-should-never-appear")
	machine := compileCredentialPlaneMachine(t, externalTelemetrySpec("wanted"))
	// "other" listed FIRST: a leak that resolves by list position rather than
	// by name would return its secret, not the named connector's.
	service, runID := newExternalTelemetryCredentialPlaneFixture(t, machine,
		externalTelemetryConnectorConfig("other", "OTHER_CONNECTOR_TOKEN"),
		externalTelemetryConnectorConfig("wanted", "WANTED_CONNECTOR_TOKEN"),
	)

	response, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{
		RunID: runID, Stage: "query-telemetry",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(response.Credentials) != 1 || response.Credentials[0].Value != "wanted-secret-0123456789" {
		t.Fatalf("credentials = %+v, want only the named connector's own secret", response.Credentials)
	}
}
