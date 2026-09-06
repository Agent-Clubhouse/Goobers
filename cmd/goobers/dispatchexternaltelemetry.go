package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/externaltelemetry"
	"github.com/goobers/goobers/internal/journal"
)

// dispatchexternaltelemetry.go is #4341's second half: `inputs.kind:
// external-telemetry` executed IN THE POD, in-process, by dispatch-exec —
// the same shape dispatchcipoll.go established for ci-poll (decision 005
// C5, #3881), extended to a kind whose executor previously HAD to be built
// from the instance's connector configuration, which a pod does not have.
//
// WHAT CLOSES THE GAP. Two facts a pod cannot fabricate, both resolved
// daemon-side and stamped at dispatch:
//
//  1. The ONE connector's non-secret ConnectorConfig (name, kind, version,
//     policy, network, provider-specific config) — internal/dispatcher's
//     externalTelemetryConnectorStamp, keyed by this stage's own
//     inputs.connector, stamped as ExternalTelemetryConnectorEnv. Never the
//     whole connector map: a pod learns only the one connector its own
//     stage names.
//  2. The connector's resolved auth secret — the SAME credential plane every
//     other pod capability resolves through, under capability.TelemetryRead
//     (credentialplane.go's resolveExternalTelemetryConnectorCredential,
//     #4341 PR 1). Never a credential REFERENCE: the value itself.
//
// Running the SAME executor.TelemetryQueryExecutor here — not a
// reimplementation — is what keeps a pod external-telemetry query and a self
// external-telemetry query one behaviour with one set of outputs, exactly as
// dispatchcipoll.go's own comment states for ci-poll.
//
// THE SUBSTITUTION POINT. A connector's factory (e.g. internal/externaltelemetry/adx)
// resolves its own credential lazily, at query time, from config.Auth.Token's
// declared Env or File name — it never receives a resolved value directly.
// The pod already HAS the resolved value (from the credential plane) but has
// neither the daemon's env var nor its file. Rather than teach every factory
// a second credential-acquisition path, this file feeds the minted secret
// through externalTelemetrySecretEnvVar and REWRITES the stamped connector's
// Auth.Token to reference exactly that variable before configuring the
// registry — the one substitution that keeps every connector factory
// unchanged on both substrates.

// externalTelemetryCapabilityUndeclaredCode names an external-telemetry
// stage that reached a pod without declaring telemetry:read. Mirrors
// ciPollCapabilityUndeclaredCode's reasoning exactly: a missing declaration
// is a WORKFLOW problem, not a credential one.
const externalTelemetryCapabilityUndeclaredCode = "capability_not_declared"

// externalTelemetryConnectorUnavailableCode names a pod that has no usable
// connector configuration to run against — the dispatcher stamped nothing
// (the named connector is not configured on this instance, or this stage
// does not name one), or the stamp fails to decode.
const externalTelemetryConnectorUnavailableCode = "external_telemetry_connector_unavailable"

// externalTelemetrySecretEnvVar is the synthetic env var name this pod
// feeds the credential-plane-minted secret through — see the substitution
// note above. It is process-local and never appears in any stamped
// ConnectorConfig; it exists only to satisfy the connector factory's own
// Env-based credential resolution.
const externalTelemetrySecretEnvVar = "GOOBERS_EXTERNAL_TELEMETRY_CONNECTOR_SECRET"

// externalTelemetryInputKeys lists every input requestFromEnvelope reads, so
// this pod's envelope carries exactly what the local path's own
// InvocationEnvelope.Inputs would for the same declared task inputs.
var externalTelemetryInputKeys = []string{
	executor.InputTelemetryConnector,
	executor.InputTelemetryQuery,
	executor.InputTelemetryQueryRef,
	executor.InputTelemetryParameters,
	executor.InputTelemetryWindow,
	executor.InputTelemetryWindowStart,
	executor.InputTelemetryWindowEnd,
	executor.InputTelemetryExpectedSchema,
	executor.InputTelemetryShape,
	executor.InputTelemetryFreshness,
	executor.InputTelemetryTimeout,
	executor.InputTelemetryMaxAttempts,
	executor.InputTelemetryRetryBackoff,
	executor.InputTelemetryMaxRows,
	executor.InputTelemetryMaxBytes,
}

// runExternalTelemetryStage executes the external-telemetry kind in-process
// in this pod and always returns a ResultEnvelope, never an error — the same
// contract runCIPollStage and runDeclaredStage follow.
func runExternalTelemetryStage(ctx context.Context, stderr io.Writer) apiv1.ResultEnvelope {
	declared, err := stageDeclaredCapabilities()
	if err != nil {
		return failureEnvelope("stage_declaration_invalid", err.Error())
	}
	required := string(capability.TelemetryRead)
	if !containsCapability(declared, required) {
		return failureEnvelope(externalTelemetryCapabilityUndeclaredCode, fmt.Sprintf(
			"stage declares inputs.kind=%s but not capability %q; external telemetry queries a named connector and cannot run without it",
			executor.KindExternalTelemetry, required,
		))
	}
	// Resolution happens HERE, at stage start, against the daemon's credential
	// plane — never at dispatch — so no secret ever rides a dispatch payload
	// or a pod spec, exactly as ci-poll's own token resolution works.
	creds, err := resolveStageCredentials(ctx)
	if err != nil {
		return failureEnvelope("credential_resolve_failed", err.Error())
	}
	secret := mintedCredentialValue(creds, required)

	connector, err := podExternalTelemetryConnector()
	if err != nil {
		return failureEnvelope(externalTelemetryConnectorUnavailableCode, err.Error())
	}

	registrar, scrubber := journal.DefaultScrubber()
	scrub := func(s string) string { return string(scrubber.Scrub([]byte(s))) }
	if secret != "" {
		// Registered BEFORE anything can carry it, exactly as ci-poll's token.
		registrar.Register([]byte(secret))
	}
	if connector.Auth.Token != nil {
		if secret == "" {
			return failureEnvelope("credential_resolve_failed", fmt.Sprintf(
				"connector %q declares an auth token but the credential plane returned no value for %q", connector.Name, required,
			))
		}
		if err := os.Setenv(externalTelemetrySecretEnvVar, secret); err != nil {
			return failureEnvelope("credential_resolve_failed", scrub(err.Error()))
		}
		connector.Auth.Token = &externaltelemetry.CredentialRef{Env: externalTelemetrySecretEnvVar}
	}

	// The SAME factory registration and Configure call the local path uses
	// (runnerwiring_executors.go), reused directly rather than restated: a
	// connector kind registered locally can never silently go
	// pod-unsupported because the pod forgot to list it too. The single-entry
	// Configuration this builds carries only the ONE connector this stage
	// named — never the instance's other connectors.
	stage, err := buildExternalTelemetryExecutor(
		externaltelemetry.Configuration{Connectors: []externaltelemetry.ConnectorConfig{connector}},
		podArtifactRecorder{stderr: stderr, scrubber: scrubber, dir: podCIPollStagingDir()},
		registrar,
	)
	if err != nil {
		return failureEnvelope(externalTelemetryConnectorUnavailableCode, scrub(err.Error()))
	}

	// The pod's own wall-clock budget — a BACKSTOP, not the query's own
	// timeout (InputTelemetryTimeout, read from the envelope), for the exact
	// reason ci-poll's own comment gives.
	runCtx, cancel := context.WithTimeout(ctx, podStageTimeout())
	defer cancel()

	result, runErr := stage.Run(runCtx, podExternalTelemetryEnvelope(declared), apiv1.DeterministicRun{})
	if runErr == nil {
		return result
	}
	return podExternalTelemetryFailure(runCtx, ctx, runErr, scrub)
}

// podExternalTelemetryConnector decodes the ONE connector the dispatcher
// stamped for this stage. Absent or undecodable is refused loudly here
// rather than defaulting to an empty connector that would fail confusingly
// deep inside registry.Configure.
func podExternalTelemetryConnector() (externaltelemetry.ConnectorConfig, error) {
	encoded := strings.TrimSpace(os.Getenv(dispatcher.ExternalTelemetryConnectorEnv))
	if encoded == "" {
		return externaltelemetry.ConnectorConfig{}, fmt.Errorf(
			"no connector configuration was stamped for this stage; the connector its inputs.connector names may not be configured on this instance",
		)
	}
	var connector externaltelemetry.ConnectorConfig
	if err := json.Unmarshal([]byte(encoded), &connector); err != nil {
		return externaltelemetry.ConnectorConfig{}, fmt.Errorf("decode %s: %w", dispatcher.ExternalTelemetryConnectorEnv, err)
	}
	return connector, nil
}

// podExternalTelemetryEnvelope rebuilds the slice of the InvocationEnvelope
// TelemetryQueryExecutor reads, from the environment the dispatcher stamped
// — an EXPLICIT list of input keys, not a sweep, for the identical reason
// podCIPollEnvelope gives.
func podExternalTelemetryEnvelope(declared []string) apiv1.InvocationEnvelope {
	inputs := map[string]interface{}{}
	for _, key := range externalTelemetryInputKeys {
		if value := strings.TrimSpace(os.Getenv(dispatcher.InputEnvVar(key))); value != "" {
			inputs[key] = value
		}
	}
	// InputTelemetryQueryRef resolves relative to Workspace; the pod's working
	// directory IS its workspace (podspec stamps WorkingDir), the same root
	// podCIPollStagingDir reports from for artifact recording.
	return apiv1.InvocationEnvelope{
		Inputs:       inputs,
		Capabilities: declared,
		Workspace:    podCIPollStagingDir(),
	}
}

// podExternalTelemetryFailure projects a TelemetryQueryExecutor error into
// the envelope this pod surrenders. Mirrors podCIPollFailure's pod-timeout
// vs parent-cancel split; external telemetry queries have no rate-limit
// class of their own; every other failure keeps its query error code as-is
// (TelemetryQueryExecutor.Run already names it "external_telemetry_<code>").
func podExternalTelemetryFailure(runCtx, parent context.Context, err error, scrub func(string) string) apiv1.ResultEnvelope {
	if parent.Err() != nil {
		return apiv1.ResultEnvelope{
			Status:  apiv1.ResultFailure,
			Summary: "external telemetry query was interrupted before it completed",
			Error: &apiv1.ErrorInfo{
				Code:      "stage_interrupted",
				Message:   scrub(err.Error()),
				Retryable: true,
			},
		}
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return apiv1.ResultEnvelope{
			Status:  apiv1.ResultFailure,
			Summary: "external telemetry query exceeded the stage's wall-clock budget",
			Error: &apiv1.ErrorInfo{
				Code:      "stage_timeout",
				Message:   scrub(err.Error()),
				Retryable: true,
			},
		}
	}
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Summary: "external telemetry query failed",
		Error: &apiv1.ErrorInfo{
			Code:    "stage_declaration_invalid",
			Message: scrub(err.Error()),
		},
	}
}
