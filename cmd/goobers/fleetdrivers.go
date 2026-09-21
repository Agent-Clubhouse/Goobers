package main

import "github.com/goobers/goobers/internal/localscheduler"

// featureDriverConfiguration observes the actual admitted starter selection,
// including a local fallback despite engine configuration. It is replaced in
// the same immutable registry snapshot as the accepted definition generation.
func featureDriverConfiguration(entries []localscheduler.WorkflowEntry) map[localscheduler.WorkflowIdentity]string {
	result := map[localscheduler.WorkflowIdentity]string{}
	if len(entries) > 1000 {
		return result
	}
	for _, entry := range entries {
		if len(result) >= 1000 {
			break
		}
		result[localscheduler.WorkflowIdentity{Gaggle: entry.Gaggle, Workflow: entry.Workflow}] = featureDriver(entry.Starter)
	}
	return result
}

func featureDriver(starter localscheduler.Starter) string {
	for range 16 {
		switch value := starter.(type) {
		case *engineStarter:
			return "runner.engine"
		case *trackedStarter:
			return "runner.local"
		case interface{ Unwrap() localscheduler.Starter }:
			starter = value.Unwrap()
		default:
			return ""
		}
	}
	return ""
}
