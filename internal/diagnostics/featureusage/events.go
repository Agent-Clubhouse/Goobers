// Package featureusage records finite operational feature observations without
// retaining prompts, request URLs, provider payloads or arbitrary configuration.
package featureusage

import (
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// Kind identifies additive, non-normative usage evidence in the run journal.
const Kind = "feature-exercised.v1"

// Appender is shared by the local journal and authenticated remote journal plane.
type Appender interface{ Append(journal.Event) error }

// AdapterID maps the adapter that actually ran, never its model or config alias.
func AdapterID(name string) string {
	switch name {
	case "copilot-cli":
		return "adapter.copilot"
	case "claude-code":
		return "adapter.claude"
	case "codex":
		return "adapter.codex"
	}
	return ""
}

// RecordAdapter records one completed adapter call, including failed calls.
// Failed preflight/materialization never reaches this hook. Append failure is
// best effort and cannot replace the invocation result; absent evidence remains
// unknown to the consumer rather than manufacturing a zero.
func RecordAdapter(recorder any, name, stage string) {
	id := AdapterID(name)
	appender, ok := recorder.(Appender)
	if !ok || id == "" {
		return
	}
	_ = appender.Append(journal.Event{Type: journal.EventRunnerAnnotation, Time: time.Now().UTC(), Stage: stage, Runner: map[string]any{"kind": Kind, "featureId": id, "count": int64(1)}})
}
