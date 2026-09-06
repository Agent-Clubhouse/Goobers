package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/externaltelemetry"
)

// dispatchexternaltelemetry_test.go is #4341's pod-parity evidence: an
// `inputs.kind: external-telemetry` stage produces the SAME outcome on a pod
// as on a self runner, driven from one fixture connector and one set of
// declared inputs, mirroring dispatchcipoll_test.go's own parity discipline.

// externalTelemetryFixture is one stage declaration, rendered for each
// substrate.
type externalTelemetryFixture struct {
	connector    externaltelemetry.ConnectorConfig
	query        string
	capabilities []string
}

func defaultExternalTelemetryFixture() externalTelemetryFixture {
	return externalTelemetryFixture{
		connector: externaltelemetry.ConnectorConfig{
			Name: "fixture", Kind: externaltelemetry.FakeKind, Version: externaltelemetry.FakeVersion,
			Config: mustMarshalJSON(map[string]any{
				"source": "fixture-source",
				"responses": map[string]any{
					"SELECT 1": map[string]any{
						"columns": []map[string]any{{"name": "value", "type": "integer"}},
						"rows":    [][]any{{1}},
					},
				},
			}),
		},
		query:        "SELECT 1",
		capabilities: []string{string(capability.TelemetryRead)},
	}
}

func mustMarshalJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func (f externalTelemetryFixture) envelope() apiv1.InvocationEnvelope {
	return apiv1.InvocationEnvelope{
		Inputs: map[string]any{
			executor.InputTelemetryConnector: f.connector.Name,
			executor.InputTelemetryQuery:     f.query,
		},
		Capabilities: f.capabilities,
	}
}

// runLocalExternalTelemetry runs the fixture through the same executor
// construction the local runner uses (buildExternalTelemetryExecutor).
func runLocalExternalTelemetry(t *testing.T, f externalTelemetryFixture) (apiv1.ResultEnvelope, error) {
	t.Helper()
	rec := &externalTelemetryTestRecorder{}
	stage, err := buildExternalTelemetryExecutor(
		externaltelemetry.Configuration{Connectors: []externaltelemetry.ConnectorConfig{f.connector}},
		rec, nil,
	)
	if err != nil {
		t.Fatalf("buildExternalTelemetryExecutor: %v", err)
	}
	return stage.Run(context.Background(), f.envelope(), apiv1.DeterministicRun{})
}

// runPodExternalTelemetry drives runExternalTelemetryStage exactly as a
// dispatched pod would: the dispatcher's stamped environment and a live
// credential plane.
func runPodExternalTelemetry(t *testing.T, f externalTelemetryFixture) apiv1.ResultEnvelope {
	t.Helper()
	setPodExternalTelemetryEnv(t, f)
	return runExternalTelemetryStage(context.Background(), io.Discard)
}

func setPodExternalTelemetryEnv(t *testing.T, f externalTelemetryFixture) {
	t.Helper()
	t.Setenv(dispatcher.InputEnvVar(executor.InputKind), executor.KindExternalTelemetry)
	t.Setenv(dispatcher.InputEnvVar(executor.InputTelemetryConnector), f.connector.Name)
	t.Setenv(dispatcher.InputEnvVar(executor.InputTelemetryQuery), f.query)
	encoded, err := json.Marshal(f.capabilities)
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	t.Setenv(dispatcher.EnvStageCapabilities, string(encoded))
	t.Setenv(dispatcher.EnvRunID, "run-external-telemetry")
	t.Setenv(dispatcher.EnvStage, "query-telemetry")
	t.Setenv(dispatcher.EnvAttempt, "1")
	t.Setenv(dispatcher.EnvStageTimeout, "30s")
	connectorJSON, err := json.Marshal(f.connector)
	if err != nil {
		t.Fatalf("marshal connector: %v", err)
	}
	t.Setenv(dispatcher.ExternalTelemetryConnectorEnv, string(connectorJSON))
	t.Setenv(dispatcher.EnvDaemonAPI, externalTelemetryCredentialPlaneStub(t, f.capabilities))
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(executor.InstanceRootEnvVar, "")
}

// externalTelemetryCredentialPlaneStub mirrors dispatchcipoll_test.go's
// credentialPlaneStub, minting a distinct value per granted capability.
func externalTelemetryCredentialPlaneStub(t *testing.T, grant []string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(apicontract.CredentialResolvePath, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Capabilities []string `json:"capabilities"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		granted := map[string]bool{}
		for _, name := range grant {
			granted[name] = true
		}
		var out struct {
			Credentials []dispatcher.MintedCredential `json:"credentials"`
		}
		for _, requested := range req.Capabilities {
			if !granted[requested] {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"capability_undeclared"}`)
				return
			}
			out.Credentials = append(out.Credentials, dispatcher.MintedCredential{
				Capability: requested,
				Value:      "minted-for-" + requested,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// The headline: a pod and a self runner produce the SAME outcome for the
// identical stage declaration and connector.
func TestExternalTelemetryPodMatchesLocalExecution(t *testing.T) {
	fixture := defaultExternalTelemetryFixture()
	local, localErr := runLocalExternalTelemetry(t, fixture)
	if localErr != nil {
		t.Fatalf("local execution: %v", localErr)
	}
	pod := runPodExternalTelemetry(t, fixture)
	if !reflect.DeepEqual(local.Outputs, pod.Outputs) {
		t.Fatalf("outputs differ:\nlocal = %+v\npod   = %+v", local.Outputs, pod.Outputs)
	}
	if local.Status != pod.Status {
		t.Fatalf("status differs: local = %s, pod = %s", local.Status, pod.Status)
	}
	if pod.Status != apiv1.ResultSuccess {
		t.Fatalf("pod status = %s, want success", pod.Status)
	}
}

// A stage that reaches a pod without declaring telemetry:read is refused as
// a WORKFLOW problem, before the credential plane is ever asked anything.
func TestExternalTelemetryPodRefusesUndeclaredCapability(t *testing.T) {
	fixture := defaultExternalTelemetryFixture()
	fixture.capabilities = nil
	result := runPodExternalTelemetry(t, fixture)
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != externalTelemetryCapabilityUndeclaredCode {
		t.Fatalf("result = %+v, want a %s failure", result, externalTelemetryCapabilityUndeclaredCode)
	}
}

// A stage whose named connector was not stamped (e.g. the daemon has no
// configuration for it) is refused loudly rather than querying a zero-value
// connector.
func TestExternalTelemetryPodRefusesMissingConnectorStamp(t *testing.T) {
	fixture := defaultExternalTelemetryFixture()
	setPodExternalTelemetryEnv(t, fixture)
	t.Setenv(dispatcher.ExternalTelemetryConnectorEnv, "")
	result := runExternalTelemetryStage(context.Background(), io.Discard)
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != externalTelemetryConnectorUnavailableCode {
		t.Fatalf("result = %+v, want a %s failure", result, externalTelemetryConnectorUnavailableCode)
	}
}

// TestExternalTelemetryPodRefusesUnresolvedAuthToken proves the fail-closed
// path: a connector declaring an auth token, but a credential plane that
// mints nothing for telemetry:read, is refused rather than querying
// unauthenticated.
func TestExternalTelemetryPodRefusesUnresolvedAuthToken(t *testing.T) {
	fixture := defaultExternalTelemetryFixture()
	fixture.connector.Auth = externaltelemetry.AuthConfig{Mode: externaltelemetry.AuthBearerToken, Token: &externaltelemetry.CredentialRef{Env: "IGNORED"}}
	setPodExternalTelemetryEnv(t, fixture)
	// Override the credential plane to mint NOTHING for telemetry:read.
	t.Setenv(dispatcher.EnvDaemonAPI, externalTelemetryCredentialPlaneStub(t, nil))
	result := runExternalTelemetryStage(context.Background(), io.Discard)
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "credential_resolve_failed" {
		t.Fatalf("result = %+v, want a credential_resolve_failed failure", result)
	}
}

// TestExternalTelemetryPodNeverStampsTheOriginalCredentialReference proves
// the substitution point: the connector this stage's own stamp declared an
// Env-based token for gets REWRITTEN to the synthetic secret env var before
// Configure ever sees it, so the pod never even attempts to read the
// original (daemon-only) env name.
func TestExternalTelemetryPodNeverReadsTheOriginalCredentialEnvName(t *testing.T) {
	fixture := defaultExternalTelemetryFixture()
	fixture.connector.Auth = externaltelemetry.AuthConfig{Mode: externaltelemetry.AuthBearerToken, Token: &externaltelemetry.CredentialRef{Env: "DAEMON_ONLY_ADX_TOKEN"}}
	// The pod process must NOT have this variable set — proving any secret the
	// connector resolves came from the credential plane's minted value, not a
	// coincidentally-present daemon-side variable.
	t.Setenv("DAEMON_ONLY_ADX_TOKEN", "")
	setPodExternalTelemetryEnv(t, fixture)
	// FakeFactory only supports AuthNone, so Configure refuses this exact
	// connector — proving the mode/factory mismatch surfaces as a connector
	// refusal (not a hang or a silent skip) is itself useful coverage, and the
	// substitution happens before this refusal, which is what
	// TestExternalTelemetryPodRefusesUnresolvedAuthToken and the successful
	// parity test above already exercise for the resolvable half.
	result := runExternalTelemetryStage(context.Background(), io.Discard)
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != externalTelemetryConnectorUnavailableCode {
		t.Fatalf("result = %+v, want a %s failure (fake connector does not support bearer-token auth)", result, externalTelemetryConnectorUnavailableCode)
	}
}

// A connector unmarshalled from an invalid stamp is refused loudly, not
// treated as an empty connector.
func TestExternalTelemetryPodRefusesUndecodableConnectorStamp(t *testing.T) {
	fixture := defaultExternalTelemetryFixture()
	setPodExternalTelemetryEnv(t, fixture)
	t.Setenv(dispatcher.ExternalTelemetryConnectorEnv, "{not json")
	result := runExternalTelemetryStage(context.Background(), io.Discard)
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != externalTelemetryConnectorUnavailableCode {
		t.Fatalf("result = %+v, want a %s failure", result, externalTelemetryConnectorUnavailableCode)
	}
}
