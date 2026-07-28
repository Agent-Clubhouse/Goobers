package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/externaltelemetry"
)

const (
	// KindExternalTelemetry selects the external telemetry built-in stage.
	KindExternalTelemetry = "external-telemetry"

	// InputTelemetryConnector and the remaining InputTelemetry* values are the
	// provider-neutral workflow input keys consumed by this stage.
	InputTelemetryConnector = "connector"
	// InputTelemetryQuery contains an inline query.
	InputTelemetryQuery = "query"
	// InputTelemetryQueryRef names a checked-in query file.
	InputTelemetryQueryRef = "queryRef"
	// InputTelemetryParameters contains a JSON parameter object.
	InputTelemetryParameters = "parameters"
	// InputTelemetryWindow contains a relative duration.
	InputTelemetryWindow = "window"
	// InputTelemetryWindowStart contains an RFC 3339 start.
	InputTelemetryWindowStart = "windowStart"
	// InputTelemetryWindowEnd contains an RFC 3339 end.
	InputTelemetryWindowEnd = "windowEnd"
	// InputTelemetryExpectedSchema contains a JSON column array.
	InputTelemetryExpectedSchema = "expectedColumns"
	// InputTelemetryShape selects point, table, or time-series.
	InputTelemetryShape = "shape"
	// InputTelemetryFreshness bounds source watermark age.
	InputTelemetryFreshness = "freshness"
	// InputTelemetryTimeout tightens the configured timeout.
	InputTelemetryTimeout = "queryTimeout"
	// InputTelemetryMaxAttempts tightens configured attempts.
	InputTelemetryMaxAttempts = "queryMaxAttempts"
	// InputTelemetryRetryBackoff declares a fixed retry delay.
	InputTelemetryRetryBackoff = "queryRetryBackoff"
	// InputTelemetryMaxRows tightens the configured row limit.
	InputTelemetryMaxRows = "maxRows"
	// InputTelemetryMaxBytes tightens the configured byte limit.
	InputTelemetryMaxBytes = "maxBytes"

	// OutputTelemetryDataState and the remaining OutputTelemetry* values are
	// scalar outputs emitted for downstream interpretation.
	OutputTelemetryDataState = "dataState"
	// OutputTelemetryQueryDigest identifies the executed query.
	OutputTelemetryQueryDigest = "queryDigest"
	// OutputTelemetryValue is the scalar value of a present point result.
	OutputTelemetryValue = "telemetryValue"

	// ExternalTelemetryArtifactName is the journal artifact emitted per query.
	ExternalTelemetryArtifactName = "external-telemetry-query.json"
)

const maxQueryFileBytes = 1 << 20

// TelemetryQueryExecutor runs the provider-neutral external telemetry host and
// records its normalized result as a journal artifact.
type TelemetryQueryExecutor struct {
	Host    *externaltelemetry.Host
	Journal BoundedArtifactRecorder
	Now     func() time.Time
}

// NewTelemetryQueryExecutor constructs the external telemetry deterministic stage.
func NewTelemetryQueryExecutor(host *externaltelemetry.Host, recorder ArtifactRecorder) (*TelemetryQueryExecutor, error) {
	if host == nil || recorder == nil {
		return nil, errors.New("executor: external telemetry host and journal are required")
	}
	boundedRecorder, ok := recorder.(BoundedArtifactRecorder)
	if !ok {
		return nil, errors.New("executor: external telemetry journal must support bounded artifacts")
	}
	return &TelemetryQueryExecutor{Host: host, Journal: boundedRecorder}, nil
}

// Run executes one external telemetry query.
func (e *TelemetryQueryExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	required := string(capability.TelemetryRead)
	if !containsString(env.Capabilities, required) {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: kind=%s requires declared capability %q", KindExternalTelemetry, required)
	}
	connectorName, err := telemetryStringInput(env, InputTelemetryConnector)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if connectorName == "" {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: kind=%s requires %s", KindExternalTelemetry, InputTelemetryConnector)
	}
	request, err := e.requestFromEnvelope(env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	artifact, queryErr := e.Host.Query(ctx, connectorName, request)
	data, err := json.Marshal(artifact)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: encode external telemetry artifact: %w", err)
	}
	ref, err := e.Journal.RecordArtifactBounded(
		ExternalTelemetryArtifactName,
		data,
		artifact.Metadata.MaxBytes,
	)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: record external telemetry artifact: %w", err)
	}
	pointer := artifactPointer(ref, "application/vnd.goobers.external-telemetry-query+json")
	outputs := map[string]any{
		OutputTelemetryDataState:   string(artifact.State),
		OutputTelemetryQueryDigest: artifact.Query.Digest,
	}
	if queryErr != nil {
		code := "query_failed"
		retryable := false
		kind := "query"
		if artifact.Failure != nil {
			code = artifact.Failure.Code
			retryable = artifact.Failure.Retryable
			kind = artifact.Failure.Kind
		}
		return apiv1.ResultEnvelope{
			Status:    apiv1.ResultFailure,
			Outputs:   outputs,
			Artifacts: []apiv1.ArtifactPointer{pointer},
			Summary:   fmt.Sprintf("external telemetry query failed: %s", code),
			Error: &apiv1.ErrorInfo{
				Code:      "external_telemetry_" + code,
				Message:   fmt.Sprintf("connector %q reported a %s failure (%s)", connectorName, kind, code),
				Retryable: retryable,
			},
		}, nil
	}
	if artifact.Shape == externaltelemetry.ShapePoint && len(artifact.Rows) == 1 && len(artifact.Rows[0]) == 1 {
		outputs[OutputTelemetryValue] = artifact.Rows[0][0]
	}
	return apiv1.ResultEnvelope{
		Status:    apiv1.ResultSuccess,
		Outputs:   outputs,
		Artifacts: []apiv1.ArtifactPointer{pointer},
		Summary:   fmt.Sprintf("external telemetry query completed with data state %s", artifact.State),
	}, nil
}

func (e *TelemetryQueryExecutor) requestFromEnvelope(env apiv1.InvocationEnvelope) (externaltelemetry.QueryRequest, error) {
	query, err := telemetryStringInput(env, InputTelemetryQuery)
	if err != nil {
		return externaltelemetry.QueryRequest{}, err
	}
	queryRef, err := telemetryStringInput(env, InputTelemetryQueryRef)
	if err != nil {
		return externaltelemetry.QueryRequest{}, err
	}
	if (query == "") == (queryRef == "") {
		return externaltelemetry.QueryRequest{}, fmt.Errorf("executor: kind=%s requires exactly one of %s or %s", KindExternalTelemetry, InputTelemetryQuery, InputTelemetryQueryRef)
	}
	if queryRef != "" {
		resolved, err := apiv1.ResolveContainedPath(env.Workspace, queryRef)
		if err != nil {
			return externaltelemetry.QueryRequest{}, fmt.Errorf("executor: resolve telemetry queryRef %q: %w", queryRef, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return externaltelemetry.QueryRequest{}, fmt.Errorf("executor: stat telemetry queryRef %q: %w", queryRef, err)
		}
		if !info.Mode().IsRegular() || info.Size() > maxQueryFileBytes {
			return externaltelemetry.QueryRequest{}, fmt.Errorf("executor: telemetry queryRef %q must be a regular file no larger than %d bytes", queryRef, maxQueryFileBytes)
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return externaltelemetry.QueryRequest{}, fmt.Errorf("executor: read telemetry queryRef %q: %w", queryRef, err)
		}
		query = string(data)
	}

	request := externaltelemetry.QueryRequest{
		Query:    query,
		QueryRef: queryRef,
		Shape:    externaltelemetry.ShapeTable,
	}
	value, err := telemetryStringInput(env, InputTelemetryShape)
	if err != nil {
		return externaltelemetry.QueryRequest{}, err
	}
	if value != "" {
		request.Shape = externaltelemetry.ResultShape(value)
	}
	value, err = telemetryStringInput(env, InputTelemetryParameters)
	if err != nil {
		return externaltelemetry.QueryRequest{}, err
	}
	if value != "" {
		if err := decodeStrictJSON(value, &request.Parameters); err != nil {
			return externaltelemetry.QueryRequest{}, fmt.Errorf("executor: invalid %s: %w", InputTelemetryParameters, err)
		}
		if request.Parameters == nil {
			return externaltelemetry.QueryRequest{}, fmt.Errorf("executor: invalid %s: expected a JSON object", InputTelemetryParameters)
		}
	}
	value, err = telemetryStringInput(env, InputTelemetryExpectedSchema)
	if err != nil {
		return externaltelemetry.QueryRequest{}, err
	}
	if value != "" {
		if err := decodeStrictJSON(value, &request.ExpectedColumns); err != nil {
			return externaltelemetry.QueryRequest{}, fmt.Errorf("executor: invalid %s: %w", InputTelemetryExpectedSchema, err)
		}
		if request.ExpectedColumns == nil {
			return externaltelemetry.QueryRequest{}, fmt.Errorf("executor: invalid %s: expected a JSON array", InputTelemetryExpectedSchema)
		}
	}
	if err := e.populateWindow(env, &request); err != nil {
		return externaltelemetry.QueryRequest{}, err
	}
	if request.Freshness, err = optionalDurationInput(env, InputTelemetryFreshness); err != nil {
		return externaltelemetry.QueryRequest{}, err
	}
	if request.Limits.Timeout, err = optionalDurationInput(env, InputTelemetryTimeout); err != nil {
		return externaltelemetry.QueryRequest{}, err
	}
	if request.Limits.RetryBackoff, err = optionalDurationInput(env, InputTelemetryRetryBackoff); err != nil {
		return externaltelemetry.QueryRequest{}, err
	}
	if request.Limits.MaxAttempts, err = optionalPositiveIntInput(env, InputTelemetryMaxAttempts); err != nil {
		return externaltelemetry.QueryRequest{}, err
	}
	if request.Limits.MaxRows, err = optionalPositiveIntInput(env, InputTelemetryMaxRows); err != nil {
		return externaltelemetry.QueryRequest{}, err
	}
	if request.Limits.MaxBytes, err = optionalPositiveIntInput(env, InputTelemetryMaxBytes); err != nil {
		return externaltelemetry.QueryRequest{}, err
	}
	return request, nil
}

func (e *TelemetryQueryExecutor) populateWindow(env apiv1.InvocationEnvelope, request *externaltelemetry.QueryRequest) error {
	relative, err := telemetryStringInput(env, InputTelemetryWindow)
	if err != nil {
		return err
	}
	start, err := telemetryStringInput(env, InputTelemetryWindowStart)
	if err != nil {
		return err
	}
	end, err := telemetryStringInput(env, InputTelemetryWindowEnd)
	if err != nil {
		return err
	}
	if relative != "" && (start != "" || end != "") {
		return fmt.Errorf("executor: %s is mutually exclusive with %s/%s", InputTelemetryWindow, InputTelemetryWindowStart, InputTelemetryWindowEnd)
	}
	if relative != "" {
		duration, err := time.ParseDuration(relative)
		if err != nil || duration <= 0 {
			return fmt.Errorf("executor: invalid %s input %q", InputTelemetryWindow, relative)
		}
		now := time.Now
		if e.Now != nil {
			now = e.Now
		}
		windowEnd := now().UTC()
		windowStart := windowEnd.Add(-duration)
		request.Window = externaltelemetry.Window{Start: &windowStart, End: &windowEnd}
		return nil
	}
	if start != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, start)
		if parseErr != nil {
			return fmt.Errorf("executor: invalid %s input %q: %w", InputTelemetryWindowStart, start, parseErr)
		}
		parsed = parsed.UTC()
		request.Window.Start = &parsed
	}
	if end != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, end)
		if parseErr != nil {
			return fmt.Errorf("executor: invalid %s input %q: %w", InputTelemetryWindowEnd, end, parseErr)
		}
		parsed = parsed.UTC()
		request.Window.End = &parsed
	}
	return nil
}

func decodeStrictJSON(value string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func optionalDurationInput(env apiv1.InvocationEnvelope, key string) (time.Duration, error) {
	value, err := telemetryStringInput(env, key)
	if err != nil {
		return 0, err
	}
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("executor: invalid %s input %q", key, value)
	}
	return duration, nil
}

func optionalPositiveIntInput(env apiv1.InvocationEnvelope, key string) (int, error) {
	value, err := telemetryStringInput(env, key)
	if err != nil {
		return 0, err
	}
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("executor: invalid %s input %q", key, value)
	}
	return parsed, nil
}

func telemetryStringInput(env apiv1.InvocationEnvelope, key string) (string, error) {
	value, present := env.Inputs[key]
	if !present {
		return "", nil
	}
	stringValue, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("executor: invalid %s input: expected string, got %T", key, value)
	}
	return stringValue, nil
}
