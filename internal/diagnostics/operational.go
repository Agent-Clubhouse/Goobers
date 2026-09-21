package diagnostics

import (
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/fleetdiagnostics"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// OperationalEvidence is a bounded, offline-readable window. It omits routing
// contacts and arbitrary payloads; operators preview the bundle before sharing.
type OperationalEvidence struct {
	Coverage     string                   `json:"coverage"`
	Observations []OperationalObservation `json:"observations,omitempty"`
	Gaps         []string                 `json:"gaps,omitempty"`
}

// OperationalObservation carries operational provenance, never work content.
type OperationalObservation struct {
	DiagnosticsDroppedRecords *int64                          `json:"diagnosticsDroppedRecords,omitempty"`
	Backlog                   *fleetdiagnostics.BacklogHealth `json:"backlog,omitempty"`
	Worker                    *fleetdiagnostics.WorkerHealth  `json:"worker,omitempty"`
	RequiredMCP               *fleetdiagnostics.MCPHealth     `json:"requiredMcp,omitempty"`
	InstanceID                string                          `json:"instanceId,omitempty"`
	GaggleID                  string                          `json:"gaggleId,omitempty"`
	Component                 string                          `json:"component,omitempty"`
	BootID                    string                          `json:"bootId,omitempty"`
	Version                   string                          `json:"version,omitempty"`
	Commit                    string                          `json:"commit,omitempty"`
	Platform                  string                          `json:"platform,omitempty"`
	ObservedAt                string                          `json:"observedAt,omitempty"`
	WindowStart               string                          `json:"windowStart,omitempty"`
	WindowCoverage            string                          `json:"windowCoverage,omitempty"`
	LastUsefulProgressAt      string                          `json:"lastUsefulProgressAt,omitempty"`
	State                     string                          `json:"state,omitempty"`
	ReasonCode                string                          `json:"reasonCode,omitempty"`
}

func collectOperationalEvidence(root string) *OperationalEvidence {
	result := &OperationalEvidence{Coverage: "unavailable"}
	events, truncated, err := journal.ReadInstanceLogWindow((instance.Layout{Root: root}).SchedulerDir(), 4<<20, 1000)
	if err != nil {
		result.Gaps = []string{"Instance operational journal could not be read; no live daemon or collector was contacted."}
		return result
	}
	result.Coverage = "retained-window"
	if truncated {
		result.Gaps = append(result.Gaps, "Only the latest 4 MiB and 1000 instance events were inspected; earlier observations may be omitted.")
	}
	latest := make(map[string]OperationalObservation)
	for _, event := range events {
		if !event.KnownSchema() || event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != "goobers.fleet.heartbeat" {
			continue
		}
		attrs, ok := event.Runner["diagnostic"].(map[string]any)
		if !ok {
			continue
		}
		schema, ok := attrs["schemaVersion"].(float64)
		if !ok || schema != 1 {
			result.Gaps = append(result.Gaps, "An unsupported diagnostic schema was omitted.")
			continue
		}
		observation := projectOperationalObservation(attrs)
		key := observation.InstanceID + "\x00" + observation.GaggleID
		if _, ok := latest[key]; !ok && len(latest) >= 100 {
			result.Coverage = "partial"
			continue
		}
		latest[key] = observation
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result.Observations = append(result.Observations, latest[key])
	}
	if len(result.Observations) == 0 {
		result.Gaps = append(result.Gaps, "No retained fleet heartbeat was observed; this does not establish deployment health or zero activity.")
	}
	if result.Coverage == "partial" {
		result.Gaps = append(result.Gaps, "Operational evidence is limited to 100 deployment/gaggle identities.")
	}
	result.Gaps = append(result.Gaps, "Observation coverage is historical; collector delivery, current reachability, raw work content, and owner contact details are not included.")
	return result
}

func projectOperationalObservation(attrs map[string]any) OperationalObservation {
	field := func(name string) string {
		value, _ := attrs[name].(string)
		if len(value) > 256 {
			return ""
		}
		return value
	}
	at, _ := time.Parse(time.RFC3339Nano, field("observedAt"))
	worker, err := fleetdiagnostics.DecodeWorkerHealth(attrs, at)
	if err != nil {
		worker = nil
	}
	return OperationalObservation{DiagnosticsDroppedRecords: projectDiagnosticDrops(attrs), Backlog: projectOperationalBacklog(attrs, at), RequiredMCP: projectOperationalMCP(attrs), Worker: worker, InstanceID: field("instanceId"), GaggleID: field("gaggleId"), Component: field("component"), BootID: field("bootId"), Version: field("version"), Commit: field("buildCommit"), Platform: field("platform"), ObservedAt: field("observedAt"), WindowStart: field("windowStart"), WindowCoverage: field("windowCoverage"), LastUsefulProgressAt: field("lastUsefulProgressAt"), State: field("state"), ReasonCode: field("reasonCode")}
}

func summaryOperational(b *strings.Builder, bundle Bundle) {
	if bundle.Operational == nil {
		return
	}
	writeLine(b, "## Operational evidence")
	writeLine(b, "Coverage: %s", bundle.Operational.Coverage)
	for _, observation := range bundle.Operational.Observations {
		writeLine(b, "- %s/%s: %s (%s), observed %s; build %s/%s; last useful progress %s.", observation.InstanceID, observation.GaggleID, observation.State, observation.ReasonCode, observation.ObservedAt, observation.Version, observation.Commit, orUnknown(observation.LastUsefulProgressAt))
		if dropped := observation.DiagnosticsDroppedRecords; dropped != nil {
			writeLine(b, "  Diagnostic records dropped by this daemon boot: %d (historical observation).", *dropped)
		}
		if backlog := observation.Backlog; backlog != nil {
			writeLine(b, "  Pending work: %s (%s), coverage %s; claim availability unknown.", backlog.State, backlog.ReasonCode, backlog.Coverage)
		}
		if mcp := observation.RequiredMCP; mcp != nil {
			writeLine(b, "  Required MCP: %s (%s), coverage %s; context %s/%s/%s.", mcp.State, mcp.Reason, mcp.Coverage, mcp.Workflow, mcp.Stage, mcp.Adapter)
		}
	}
	for _, gap := range bundle.Operational.Gaps {
		writeLine(b, "- Gap: %s", gap)
	}
	writeLine(b, "")
}

func projectOperationalMCP(attrs map[string]any) *fleetdiagnostics.MCPHealth {
	raw, _ := attrs["observedAt"].(string)
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil
	}
	health, err := fleetdiagnostics.DecodeMCPHealth(attrs, at)
	if err != nil {
		return nil
	}
	return health
}

func projectOperationalBacklog(attrs map[string]any, at time.Time) *fleetdiagnostics.BacklogHealth {
	health, err := fleetdiagnostics.DecodeBacklogHealth(attrs, at)
	if err != nil {
		return nil
	}
	return health
}

func projectDiagnosticDrops(attrs map[string]any) *int64 {
	value, ok := attrs["diagnosticsDroppedRecords"]
	if !ok {
		return nil
	}
	switch count := value.(type) {
	case int64:
		if count >= 0 {
			return &count
		}
	case float64:
		// JSON instance journal numbers must be exactly representable integers.
		if count >= 0 && count < 1<<53 && count == float64(int64(count)) {
			result := int64(count)
			return &result
		}
	}
	return nil
}
