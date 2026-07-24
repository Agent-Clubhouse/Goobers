package telemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.opentelemetry.io/otel/attribute"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	// StageTelemetryEnv is the environment variable stages use to find their
	// zero-dependency telemetry emission directory.
	StageTelemetryEnv = "GOOBERS_TELEMETRY_DIR"

	metricsFile  = "metrics.jsonl"
	eventsFile   = "events.jsonl"
	stageDirName = "telemetry"

	emissionKindAttribute = "goobers.telemetry.kind"
	metricValueAttribute  = "goobers.metric.value"
	metricUnitAttribute   = "goobers.metric.unit"
	warningFileAttribute  = "goobers.telemetry.file"
	warningCountAttribute = "goobers.telemetry.dropped_lines"

	emissionKindMetric = "metric"
	emissionKindEvent  = "event"
	warningEventName   = "goobers.telemetry.warning"

	maxEmissionLineBytes  = 1 << 20
	maxEmissionFileBytes  = 8 << 20
	maxEmissionRecords    = 10_000
	maxEmissionAttributes = 125

	// Reserve three attributes for metric metadata (kind, value, and unit).
	// Events require only kind, so the same limit is safe for both record types.
	maxSpanEventAttributes = maxEmissionAttributes + 3
	maxSpanEvents          = 2*maxEmissionRecords + 2
)

type stageMetric struct {
	Name  string         `json:"name"`
	Value *float64       `json:"value"`
	Unit  string         `json:"unit,omitempty"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

type stageEvent struct {
	Time  time.Time      `json:"ts"`
	Name  string         `json:"name"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// StageTelemetryDir returns the stage-scoped emission directory for workspace.
func StageTelemetryDir(workspace string) string {
	if workspace == "" {
		return ""
	}
	return filepath.Join(filepath.Clean(workspace), ".goobers", stageDirName)
}

// PrepareStageTelemetryDir creates and returns the stage-scoped emission
// directory. Directory creation is best-effort because optional telemetry must
// never turn a stage into a failure.
func PrepareStageTelemetryDir(workspace string) string {
	dir := StageTelemetryDir(workspace)
	if dir == "" {
		return ""
	}
	if !ensurePlainDir(filepath.Dir(dir)) || !ensurePlainDir(dir) {
		return ""
	}
	_ = os.Chmod(dir, 0o700)
	return dir
}

// ResetStageTelemetryDir removes stale emissions from an earlier attempt and
// prepares a fresh directory before the stage process starts.
func ResetStageTelemetryDir(workspace string) string {
	dir := StageTelemetryDir(workspace)
	if dir == "" {
		return ""
	}
	if !ensurePlainDir(filepath.Dir(dir)) {
		return ""
	}
	_ = os.RemoveAll(dir)
	return PrepareStageTelemetryDir(workspace)
}

// CleanupStageTelemetryDir removes a stage's telemetry directory.
func CleanupStageTelemetryDir(dir string) {
	parent := filepath.Dir(dir)
	if dir != "" && filepath.Base(dir) == stageDirName && filepath.Base(parent) == ".goobers" && isPlainDir(parent) {
		_ = os.RemoveAll(dir)
	}
}

// IngestStageEmissions reads a completed stage's optional JSONL emissions,
// merges custom metrics without replacing executor-computed values, and
// attaches every valid record to the stage span. Invalid records are dropped
// and summarized by a warning event; no read or decode error escapes.
func IngestStageEmissions(dir string, result *apiv1.ResultEnvelope, span Span) {
	if dir == "" || !isPlainDir(filepath.Dir(dir)) || !isPlainDir(dir) {
		return
	}
	metrics, droppedMetrics := readEmissionFile[stageMetric](filepath.Join(dir, metricsFile), validMetric)
	events, droppedEvents := readEmissionFile[stageEvent](filepath.Join(dir, eventsFile), validEvent)

	runnerMetrics := make(map[string]struct{})
	if result != nil {
		for name := range result.Metrics {
			runnerMetrics[name] = struct{}{}
		}
		if len(metrics) > 0 && result.Metrics == nil {
			result.Metrics = make(map[string]float64, len(metrics))
		}
	}

	for _, metric := range metrics {
		if result != nil {
			if _, runnerComputed := runnerMetrics[metric.Name]; !runnerComputed {
				result.Metrics[metric.Name] = *metric.Value
			}
		}
		attrs := emissionAttributes(metric.Attrs)
		attrs = append(attrs,
			attribute.String(emissionKindAttribute, emissionKindMetric),
			attribute.Float64(metricValueAttribute, *metric.Value),
		)
		if metric.Unit != "" {
			attrs = append(attrs, attribute.String(metricUnitAttribute, metric.Unit))
		}
		span.Event(metric.Name, attrs...)
	}

	for _, event := range events {
		attrs := append(emissionAttributes(event.Attrs), attribute.String(emissionKindAttribute, emissionKindEvent))
		span.EventAt(event.Time, event.Name, attrs...)
	}

	if droppedMetrics > 0 {
		span.Event(warningEventName,
			attribute.String(warningFileAttribute, metricsFile),
			attribute.Int(warningCountAttribute, droppedMetrics),
		)
	}
	if droppedEvents > 0 {
		span.Event(warningEventName,
			attribute.String(warningFileAttribute, eventsFile),
			attribute.Int(warningCountAttribute, droppedEvents),
		)
	}
}

func ensurePlainDir(path string) bool {
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		return os.Mkdir(path, 0o700) == nil
	case err != nil:
		return false
	default:
		return info.IsDir() && info.Mode()&os.ModeSymlink == 0
	}
}

func isPlainDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func validMetric(metric stageMetric) bool {
	return metric.Name != "" &&
		!IsCanonicalAgentUsageMetric(metric.Name) &&
		metric.Value != nil &&
		!math.IsNaN(*metric.Value) &&
		!math.IsInf(*metric.Value, 0) &&
		len(metric.Attrs) <= maxEmissionAttributes
}

func validEvent(event stageEvent) bool {
	return event.Name != "" && !event.Time.IsZero() && len(event.Attrs) <= maxEmissionAttributes
}

func readEmissionFile[T any](path string, valid func(T) bool) ([]T, int) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, 0
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, 1
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, 1
	}
	defer func() { _ = file.Close() }()

	var records []T
	dropped := 0
	reader := bufio.NewReaderSize(io.LimitReader(file, maxEmissionFileBytes), 64*1024)
	fileTruncated := info.Size() > maxEmissionFileBytes
	for {
		line, oversized, readErr := readEmissionLine(reader)
		if oversized {
			dropped++
		} else if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			if errors.Is(readErr, io.EOF) && fileTruncated {
				return records, dropped + 1
			}
			if len(records) >= maxEmissionRecords {
				dropped++
			} else {
				var record T
				decoder := json.NewDecoder(bytes.NewReader(trimmed))
				if err := decoder.Decode(&record); err != nil || decoder.Decode(&struct{}{}) != io.EOF || !valid(record) {
					dropped++
				} else {
					records = append(records, record)
				}
			}
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			if fileTruncated {
				dropped++
			}
			return records, dropped
		}
		return records, dropped + 1
	}
}

func readEmissionLine(reader *bufio.Reader) ([]byte, bool, error) {
	line := make([]byte, 0, 1024)
	oversized := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if !oversized {
			if len(line)+len(fragment) > maxEmissionLineBytes {
				line = nil
				oversized = true
			} else {
				line = append(line, fragment...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, oversized, err
	}
}

func emissionAttributes(values map[string]any) []attribute.KeyValue {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	attrs := make([]attribute.KeyValue, 0, len(keys))
	for _, key := range keys {
		switch value := values[key].(type) {
		case string:
			attrs = append(attrs, attribute.String(key, value))
		case bool:
			attrs = append(attrs, attribute.Bool(key, value))
		case float64:
			attrs = append(attrs, attribute.Float64(key, value))
		case nil:
			attrs = append(attrs, attribute.String(key, "null"))
		default:
			if encoded, err := json.Marshal(value); err == nil {
				attrs = append(attrs, attribute.String(key, string(encoded)))
			}
		}
	}
	return attrs
}
