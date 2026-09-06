package dispatcher

import (
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/externaltelemetry"
)

// externaltelemetryenv_test.go is #4341's dispatcher-side evidence: the ONE
// connector a stage's inputs.connector names is resolved HERE, where the
// instance config is readable, and stamped as non-secret JSON — never the
// whole connector map, and never the credential reference's actual value
// (the pod resolves that through the credential plane instead).

// TestExternalTelemetryConnectorEnvNamesMatchExecutor pins podspec.go's
// restated "kind"/"external-telemetry"/"connector" string literals against
// their originals in internal/executor — the same discipline
// TestRunContextEnvMatchesExecutor applies to the run-context names. If this
// fails, a rename in executor has silently stopped this stamp from firing.
func TestExternalTelemetryConnectorEnvNamesMatchExecutor(t *testing.T) {
	if executorInputKindKey != executor.InputKind {
		t.Errorf("executorInputKindKey = %q, want executor.InputKind %q", executorInputKindKey, executor.InputKind)
	}
	if executorKindExternalTelemetry != executor.KindExternalTelemetry {
		t.Errorf("executorKindExternalTelemetry = %q, want executor.KindExternalTelemetry %q", executorKindExternalTelemetry, executor.KindExternalTelemetry)
	}
	if executorInputTelemetryConnector != executor.InputTelemetryConnector {
		t.Errorf("executorInputTelemetryConnector = %q, want executor.InputTelemetryConnector %q", executorInputTelemetryConnector, executor.InputTelemetryConnector)
	}
}

// externalTelemetryConfig is a dispatcher wired the way workerdispatch.go
// wires it from an instance with two configured connectors.
func externalTelemetryConfig() Config {
	cfg := testConfig()
	cfg.ExternalTelemetryConnectors = map[string]externaltelemetry.ConnectorConfig{
		"adx-prod": {
			Name: "adx-prod", Kind: "adx", Version: "v1",
			Auth:   externaltelemetry.AuthConfig{Mode: "bearer-token", Token: &externaltelemetry.CredentialRef{Env: "ADX_TOKEN"}},
			Config: json.RawMessage(`{"cluster":"https://example.kusto.windows.net","database":"telemetry"}`),
		},
		"other-connector": {
			Name: "other-connector", Kind: "fake", Version: "v1",
			Config: json.RawMessage(`{}`),
		},
	}
	return cfg
}

// externalTelemetryAttempt is a non-CLI deterministic stage declaring
// inputs.kind=external-telemetry and naming adx-prod.
func externalTelemetryAttempt() Attempt {
	attempt := testAttempt()
	attempt.Inputs = map[string]string{
		executor.InputKind:               executor.KindExternalTelemetry,
		executor.InputTelemetryConnector: "adx-prod",
	}
	return attempt
}

// The headline: a rendered external-telemetry stage pod carries its named
// connector's non-secret configuration, and only that one connector's.
func TestRenderedExternalTelemetryStagePodCarriesItsNamedConnector(t *testing.T) {
	pod, err := RenderPod(externalTelemetryConfig(), externalTelemetryAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	raw, stamped := podEnvMap(pod)[ExternalTelemetryConnectorEnv]
	if !stamped {
		t.Fatalf("%s was not stamped", ExternalTelemetryConnectorEnv)
	}
	var connector externaltelemetry.ConnectorConfig
	if err := json.Unmarshal([]byte(raw), &connector); err != nil {
		t.Fatalf("decode %s: %v", ExternalTelemetryConnectorEnv, err)
	}
	if connector.Name != "adx-prod" || connector.Kind != "adx" {
		t.Fatalf("stamped connector = %+v, want the named adx-prod connector", connector)
	}
	if connector.Auth.Token != nil {
		t.Fatalf("stamped connector carries Auth.Token = %+v; the pod must resolve the secret through the credential plane, not a stamped reference", connector.Auth.Token)
	}
}

// A stage naming a connector this process has no configuration for gets NO
// stamp at all — the pod's own executor construction refuses loudly on a
// missing stamp rather than this function guessing at a diagnostic it
// cannot deliver.
func TestExternalTelemetryConnectorStampAbsentForUnknownConnector(t *testing.T) {
	attempt := externalTelemetryAttempt()
	attempt.Inputs[executor.InputTelemetryConnector] = "removed-connector"
	pod, err := RenderPod(externalTelemetryConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if value, stamped := podEnvMap(pod)[ExternalTelemetryConnectorEnv]; stamped {
		t.Fatalf("%s = %q for an unconfigured connector, want no stamp", ExternalTelemetryConnectorEnv, value)
	}
}

// A stage of any other kind never sees this stamp, including one that
// happens to declare an inputs.connector value.
func TestExternalTelemetryConnectorStampAbsentForOtherKinds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inputs map[string]string
	}{
		{"shell stage", map[string]string{}},
		{"ci-poll stage", map[string]string{executor.InputKind: executor.KindCIPoll}},
		{"connector named with no kind", map[string]string{executor.InputTelemetryConnector: "adx-prod"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt := testAttempt()
			attempt.Inputs = tc.inputs
			pod, err := RenderPod(externalTelemetryConfig(), attempt, linuxRunner())
			if err != nil {
				t.Fatalf("RenderPod: %v", err)
			}
			if value, stamped := podEnvMap(pod)[ExternalTelemetryConnectorEnv]; stamped {
				t.Fatalf("%s = %q, want no stamp", ExternalTelemetryConnectorEnv, value)
			}
		})
	}
}

// No connectors configured at all is the common case and must not panic or
// stamp a zero-value connector.
func TestExternalTelemetryConnectorStampAbsentWithNoConnectorsConfigured(t *testing.T) {
	cfg := testConfig()
	pod, err := RenderPod(cfg, externalTelemetryAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if value, stamped := podEnvMap(pod)[ExternalTelemetryConnectorEnv]; stamped {
		t.Fatalf("%s = %q with no connectors configured, want no stamp", ExternalTelemetryConnectorEnv, value)
	}
}
