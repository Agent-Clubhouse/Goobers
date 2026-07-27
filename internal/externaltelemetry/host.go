package externaltelemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"
)

// Host executes connectors while enforcing time, retry, schema, row, and byte
// policy independently of adapter behavior.
type Host struct {
	Registry *Registry
	Now      func() time.Time
	Sleep    func(context.Context, time.Duration) error
	Progress func()
}

// Query executes one named connector and always returns a versioned artifact,
// including a non-secret failed artifact when err is non-nil.
func (h *Host) Query(ctx context.Context, connectorName string, request QueryRequest) (ResultArtifact, error) {
	started := h.now()()
	provenance, provenanceErr := queryProvenance(request)
	if provenanceErr != nil {
		queryErr := classifyError(NewQueryError("invalid_parameters", "configuration", false, provenanceErr))
		return failedArtifact(connectorName, Descriptor{}, request, provenance, started, h.now()(), 0, QueryLimits{}, queryErr), queryErr
	}
	if h.Registry == nil {
		queryErr := classifyError(NewQueryError("host_not_configured", "configuration", false, errors.New("external telemetry registry is nil")))
		return failedArtifact(connectorName, Descriptor{}, request, provenance, started, h.now()(), 0, QueryLimits{}, queryErr), queryErr
	}
	entry, err := h.Registry.connector(connectorName)
	if err != nil {
		queryErr := classifyError(err)
		return failedArtifact(connectorName, Descriptor{}, request, provenance, started, h.now()(), 0, QueryLimits{}, queryErr), queryErr
	}
	limits := effectiveLimits(entry.limits, request.Limits)
	if err := validateRequest(request, entry.shapes); err != nil {
		queryErr := classifyError(err)
		return failedArtifact(connectorName, entry.connector.Descriptor(), request, provenance, started, h.now()(), 0, limits, queryErr), queryErr
	}

	queryCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()
	sleep := h.Sleep
	if sleep == nil {
		sleep = contextSleep
	}

	var source SourceResult
	attempts := 0
	for attempts < limits.MaxAttempts {
		attempts++
		source, err = entry.connector.Query(queryCtx, requestWithLimits(request, limits))
		if h.Progress != nil {
			h.Progress()
		}
		if err == nil {
			break
		}
		queryErr := classifyError(err)
		if !queryErr.Retryable || attempts == limits.MaxAttempts {
			ended := h.now()()
			return failedArtifact(connectorName, entry.connector.Descriptor(), request, provenance, started, ended, attempts, limits, queryErr), queryErr
		}
		if sleepErr := sleep(queryCtx, limits.RetryBackoff); sleepErr != nil {
			queryErr = classifyError(sleepErr)
			ended := h.now()()
			return failedArtifact(connectorName, entry.connector.Descriptor(), request, provenance, started, ended, attempts, limits, queryErr), queryErr
		}
	}

	ended := h.now()()
	artifact, normalizeErr := normalize(connectorName, entry.connector.Descriptor(), request, provenance, source, started, ended, attempts, limits)
	if normalizeErr != nil {
		queryErr := classifyError(normalizeErr)
		return failedArtifact(connectorName, entry.connector.Descriptor(), request, provenance, started, ended, attempts, limits, queryErr), queryErr
	}
	return artifact, nil
}

func normalize(
	name string,
	descriptor Descriptor,
	request QueryRequest,
	provenance QueryProvenance,
	source SourceResult,
	started, ended time.Time,
	attempts int,
	limits QueryLimits,
) (ResultArtifact, error) {
	if err := validateSourceResult(source, request.Shape); err != nil {
		return ResultArtifact{}, err
	}
	if err := validateExpectedColumns(source.Columns, request.ExpectedColumns); err != nil {
		return ResultArtifact{}, err
	}
	artifact := ResultArtifact{
		SchemaVersion: ResultSchemaVersion,
		Connector:     ConnectorIdentity{Name: name, Kind: descriptor.Kind, Version: descriptor.Version},
		Source:        SourceIdentity{ID: descriptor.SourceID},
		Query:         provenance,
		Window:        request.Window,
		QueryStarted:  started.UTC(),
		QueryEnded:    ended.UTC(),
		DataAsOf:      source.DataAsOf,
		State:         dataState(source, request.Freshness, ended),
		Shape:         request.Shape,
		Columns:       append([]Column(nil), source.Columns...),
		Rows:          cloneRows(source.Rows),
		Labels:        cloneLabels(source.Labels),
		Metadata: ResultMetadata{
			Attempts:  attempts,
			Sampled:   source.Sampled,
			Truncated: source.Truncated,
			MaxRows:   limits.MaxRows,
			MaxBytes:  limits.MaxBytes,
		},
	}
	if len(artifact.Rows) > limits.MaxRows {
		artifact.Metadata.RowsDropped = len(artifact.Rows) - limits.MaxRows
		artifact.Rows = artifact.Rows[:limits.MaxRows]
		artifact.Metadata.Truncated = true
	}
	for {
		data, err := json.Marshal(artifact)
		if err != nil {
			return ResultArtifact{}, NewQueryError("normalization_failed", "normalization", false, err)
		}
		if len(data) <= limits.MaxBytes {
			break
		}
		if len(artifact.Rows) == 0 {
			return ResultArtifact{}, NewQueryError(
				"result_too_large",
				"limit",
				false,
				fmt.Errorf("normalized metadata exceeds %d-byte limit", limits.MaxBytes),
			)
		}
		artifact.Rows = artifact.Rows[:len(artifact.Rows)-1]
		artifact.Metadata.RowsDropped++
		artifact.Metadata.Truncated = true
	}
	return artifact, nil
}

func failedArtifact(
	name string,
	descriptor Descriptor,
	request QueryRequest,
	provenance QueryProvenance,
	started, ended time.Time,
	attempts int,
	limits QueryLimits,
	queryErr *QueryError,
) ResultArtifact {
	return ResultArtifact{
		SchemaVersion: ResultSchemaVersion,
		Connector:     ConnectorIdentity{Name: name, Kind: descriptor.Kind, Version: descriptor.Version},
		Source:        SourceIdentity{ID: descriptor.SourceID},
		Query:         provenance,
		Window:        request.Window,
		QueryStarted:  started.UTC(),
		QueryEnded:    ended.UTC(),
		State:         DataFailed,
		Shape:         request.Shape,
		Columns:       []Column{},
		Rows:          [][]any{},
		Metadata: ResultMetadata{
			Attempts: attempts,
			MaxRows:  limits.MaxRows,
			MaxBytes: limits.MaxBytes,
		},
		Failure: &FailureInfo{Code: queryErr.Code, Kind: queryErr.Kind, Retryable: queryErr.Retryable},
	}
}

func validateRequest(request QueryRequest, supportedShapes []ResultShape) error {
	if strings.TrimSpace(request.Query) == "" {
		return NewQueryError("query_required", "configuration", false, errors.New("external telemetry query is required"))
	}
	if !validResultShape(request.Shape) {
		return NewQueryError("invalid_shape", "configuration", false, fmt.Errorf("unsupported result shape %q", request.Shape))
	}
	if !slices.Contains(supportedShapes, request.Shape) {
		return NewQueryError("unsupported_shape", "configuration", false, fmt.Errorf("connector does not support result shape %q", request.Shape))
	}
	if request.Freshness < 0 {
		return NewQueryError("invalid_freshness", "configuration", false, errors.New("freshness must not be negative"))
	}
	if request.Window.Start != nil && request.Window.End != nil && request.Window.End.Before(*request.Window.Start) {
		return NewQueryError("invalid_window", "configuration", false, errors.New("window end precedes start"))
	}
	return nil
}

func validResultShape(shape ResultShape) bool {
	switch shape {
	case ShapePoint, ShapeTable, ShapeTimeSeries:
		return true
	default:
		return false
	}
}

func validateSourceResult(result SourceResult, shape ResultShape) error {
	seen := make(map[string]struct{}, len(result.Columns))
	for i, column := range result.Columns {
		if column.Name == "" || !validColumnType(column.Type) {
			return NewQueryError("invalid_source_schema", "normalization", false, fmt.Errorf("column %d has invalid name or type", i))
		}
		if _, exists := seen[column.Name]; exists {
			return NewQueryError("invalid_source_schema", "normalization", false, fmt.Errorf("duplicate column %q", column.Name))
		}
		seen[column.Name] = struct{}{}
	}
	for rowIndex, row := range result.Rows {
		if len(row) != len(result.Columns) {
			return NewQueryError("invalid_source_rows", "normalization", false, fmt.Errorf("row %d has %d cells for %d columns", rowIndex, len(row), len(result.Columns)))
		}
		for columnIndex, value := range row {
			if !valueMatches(result.Columns[columnIndex].Type, value) {
				return NewQueryError("invalid_source_value", "normalization", false, fmt.Errorf("row %d column %q does not match type %q", rowIndex, result.Columns[columnIndex].Name, result.Columns[columnIndex].Type))
			}
		}
	}
	if shape == ShapePoint && (len(result.Rows) > 1 || len(result.Columns) > 1) {
		return NewQueryError("point_cardinality", "schema", false, errors.New("point result must contain at most one row and one column"))
	}
	if shape == ShapeTimeSeries && !slices.ContainsFunc(result.Columns, func(column Column) bool {
		return column.Type == TypeDateTime
	}) {
		return NewQueryError("time_series_timestamp_missing", "schema", false, errors.New("time-series result requires a datetime column"))
	}
	return nil
}

func validateExpectedColumns(actual, expected []Column) error {
	if len(expected) == 0 {
		return nil
	}
	if !reflect.DeepEqual(actual, expected) {
		return NewQueryError(
			"schema_mismatch",
			"schema",
			false,
			fmt.Errorf("actual columns %#v do not match expected columns %#v", actual, expected),
		)
	}
	return nil
}

func validColumnType(value ColumnType) bool {
	switch value {
	case TypeString, TypeInteger, TypeNumber, TypeBoolean, TypeDateTime, TypeDuration, TypeJSON:
		return true
	default:
		return false
	}
}

func valueMatches(columnType ColumnType, value any) bool {
	if value == nil {
		return true
	}
	switch columnType {
	case TypeString, TypeDuration:
		_, ok := value.(string)
		return ok
	case TypeDateTime:
		text, ok := value.(string)
		if !ok {
			return false
		}
		_, err := time.Parse(time.RFC3339Nano, text)
		return err == nil
	case TypeInteger:
		switch typed := value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
			if number, ok := typed.(json.Number); ok {
				_, err := number.Int64()
				return err == nil
			}
			return true
		default:
			return false
		}
	case TypeNumber:
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
			return true
		default:
			return false
		}
	case TypeBoolean:
		_, ok := value.(bool)
		return ok
	case TypeJSON:
		return true
	default:
		return false
	}
}

func dataState(result SourceResult, freshness time.Duration, now time.Time) DataState {
	if len(result.Rows) == 0 {
		return DataEmpty
	}
	if freshness > 0 && (result.DataAsOf == nil || result.DataAsOf.Before(now.Add(-freshness))) {
		return DataStale
	}
	return DataPresent
}

func effectiveLimits(maximum, requested QueryLimits) QueryLimits {
	effective := maximum
	if requested.Timeout > 0 && requested.Timeout < effective.Timeout {
		effective.Timeout = requested.Timeout
	}
	if requested.MaxAttempts > 0 && requested.MaxAttempts < effective.MaxAttempts {
		effective.MaxAttempts = requested.MaxAttempts
	}
	if requested.RetryBackoff > 0 && requested.RetryBackoff > effective.RetryBackoff {
		effective.RetryBackoff = requested.RetryBackoff
	}
	if requested.MaxRows > 0 && requested.MaxRows < effective.MaxRows {
		effective.MaxRows = requested.MaxRows
	}
	if requested.MaxBytes > 0 && requested.MaxBytes < effective.MaxBytes {
		effective.MaxBytes = requested.MaxBytes
	}
	return effective
}

func requestWithLimits(request QueryRequest, limits QueryLimits) QueryRequest {
	request.Limits = limits
	return request
}

func queryProvenance(request QueryRequest) (QueryProvenance, error) {
	parameters, err := json.Marshal(request.Parameters)
	if err != nil {
		return QueryProvenance{}, fmt.Errorf("encode query parameters: %w", err)
	}
	names := make([]string, 0, len(request.Parameters))
	for name := range request.Parameters {
		names = append(names, name)
	}
	sort.Strings(names)
	parameterSum := sha256.Sum256(parameters)
	queryInput, err := json.Marshal(struct {
		Query      string
		Parameters json.RawMessage
		Window     Window
	}{Query: request.Query, Parameters: parameters, Window: request.Window})
	if err != nil {
		return QueryProvenance{}, fmt.Errorf("encode query identity: %w", err)
	}
	querySum := sha256.Sum256(queryInput)
	provenance := QueryProvenance{
		Digest:         "sha256:" + hex.EncodeToString(querySum[:]),
		Reference:      request.QueryRef,
		ParameterNames: names,
	}
	if len(request.Parameters) > 0 {
		provenance.ParameterDigest = "sha256:" + hex.EncodeToString(parameterSum[:])
	}
	return provenance, nil
}

func cloneRows(rows [][]any) [][]any {
	cloned := make([][]any, len(rows))
	for i := range rows {
		cloned[i] = append([]any(nil), rows[i]...)
	}
	return cloned
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func (h *Host) now() func() time.Time {
	if h.Now != nil {
		return h.Now
	}
	return time.Now
}

func contextSleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
