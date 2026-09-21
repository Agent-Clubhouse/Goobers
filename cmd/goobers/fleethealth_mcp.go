package main

import (
	"sort"
	"time"

	"github.com/goobers/goobers/internal/fleetdiagnostics"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

const fleetMCPContextLimit = 256

type fleetMCPContext struct {
	workflow, stage, adapter, server string
	branch                           int
}

// A latest per-run unknown check is not a recovery. Fold the independent
// decisive evidence clocks across runs of the same workflow/stage/branch.
func fleetMCPHealth(runs []readservice.RunSummary, gaggle string, complete bool, now time.Time) fleetdiagnostics.MCPHealth {
	contexts := make(map[fleetMCPContext]readmodel.RequiredMCPCondition)
	for _, run := range runs {
		if run.Gaggle != gaggle {
			complete = false
			continue
		}
		if run.RequiredMCP == nil {
			complete = false // Legacy/uninstrumented runs cannot establish complete coverage.
			continue
		}
		if run.RequiredMCP.Truncated {
			complete = false
		}
		for _, condition := range run.RequiredMCP.Conditions {
			if condition.ObservedAt.IsZero() || condition.ObservedAt.After(now) {
				complete = false
				continue
			}
			key := fleetMCPContext{run.Workflow, condition.Stage, condition.Adapter, condition.Server, condition.Branch}
			prior, exists := contexts[key]
			if !exists && len(contexts) >= fleetMCPContextLimit {
				complete = false
				continue
			}
			contexts[key] = readmodel.MergeRequiredMCPCondition(prior, condition)
		}
	}
	return summarizeFleetMCP(contexts, complete)
}

func summarizeFleetMCP(contexts map[fleetMCPContext]readmodel.RequiredMCPCondition, complete bool) fleetdiagnostics.MCPHealth {
	result := fleetdiagnostics.MCPHealth{State: "unknown", Coverage: "unknown"}
	if len(contexts) == 0 {
		return result
	}
	result.Coverage = "partial"
	keys := make([]fleetMCPContext, 0, len(contexts))
	for key := range contexts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return fleetMCPKeyLess(keys[i], keys[j]) })
	var count int64
	known := true
	var selected *readmodel.RequiredMCPCondition
	for _, key := range keys {
		condition := contexts[key]
		if condition.AuthorizationObservedAt.IsZero() || condition.AvailabilityObservedAt.IsZero() {
			known = false
		}
		if condition.Active {
			count++
		}
		if selected == nil || condition.Active && !selected.Active || condition.Active == selected.Active && condition.ObservedAt.After(selected.ObservedAt) {
			selected = &condition
			result.Workflow, result.Stage, result.Adapter, result.Branch = key.workflow, key.stage, key.adapter, key.branch
		}
	}
	if complete && known {
		result.Coverage = "complete"
	}
	at := fleetMCPDecisiveTime(*selected)
	result.ObservedAt = &at
	if count > 0 {
		result.State, result.Reason, result.ActiveCount = "active", selected.Reason, &count
	} else if result.Coverage == "complete" {
		result.State, result.ActiveCount = "recovered", &count
	}
	return result
}

func fleetMCPKeyLess(a, b fleetMCPContext) bool {
	if a.workflow != b.workflow {
		return a.workflow < b.workflow
	}
	if a.stage != b.stage {
		return a.stage < b.stage
	}
	if a.branch != b.branch {
		return a.branch < b.branch
	}
	if a.adapter != b.adapter {
		return a.adapter < b.adapter
	}
	return a.server < b.server
}

func fleetMCPAttributes(attrs map[string]any, health fleetdiagnostics.MCPHealth) {
	attrs["requiredMcpState"], attrs["requiredMcpCoverage"] = health.State, health.Coverage
	if health.ObservedAt == nil {
		return
	}
	attrs["requiredMcpObservedAt"] = health.ObservedAt.UTC().Format(time.RFC3339Nano)
	attrs["requiredMcpReason"], attrs["requiredMcpAdapter"] = health.Reason, health.Adapter
	attrs["requiredMcpWorkflow"], attrs["requiredMcpStage"], attrs["requiredMcpBranch"] = health.Workflow, health.Stage, health.Branch
	if health.ActiveCount != nil {
		attrs["requiredMcpActiveCount"] = *health.ActiveCount
	}
}

func fleetMCPDecisiveTime(condition readmodel.RequiredMCPCondition) time.Time {
	if condition.AuthorizationReason != "" {
		return condition.AuthorizationObservedAt
	}
	if condition.AvailabilityReason != "" {
		return condition.AvailabilityObservedAt
	}
	at := condition.AuthorizationObservedAt
	if condition.AvailabilityObservedAt.After(at) {
		at = condition.AvailabilityObservedAt
	}
	if at.IsZero() {
		at = condition.ObservedAt
	}
	return at
}
