package harness

// ConformanceCoveredAdapterNames returns the Adapter.Name() values this
// package's conformance suite (conformance_test.go) proves hold across every
// capability dimension: tool allowlist, declared MCP servers, goobers-io,
// model/option validation, and ambient-config isolation (#2776). Exported so
// other packages can hold their own adapter registration point accountable —
// see cmd/goobers's TestBuildHarnessRegistryAdaptersAreConformanceCovered,
// which fails if a registered adapter isn't in this list, or vice versa.
// Every feature since the second adapter landed (tools allowlist #1471,
// goobers-io #2774, declared mcpServers #1492) went into one adapter first
// and needed a separate follow-up issue for the other, unnoticed for weeks —
// this pair of checks is the tactical guard against that recurring: nothing
// can be added to the harness registry without also being conformance-tested.
func ConformanceCoveredAdapterNames() []string {
	return []string{"copilot-cli", "claude-code"}
}
