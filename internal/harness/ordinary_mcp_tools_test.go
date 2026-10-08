package harness

// Fixed ordinary-tool fixtures preserve the adapter's established wire names.
// They are intentionally independent of the request-sensitive production helper.
func goobersIOClaudeToolNames() []string {
	return []string{
		"mcp__goobers-io__get_run_info", "mcp__goobers-io__publish_output",
		"mcp__goobers-io__list_inputs", "mcp__goobers-io__read_input", "mcp__goobers-io__grep_input",
	}
}

func goobersIOAvailableToolNames() []string {
	return []string{
		"goobers-io-get_run_info", "goobers-io-publish_output",
		"goobers-io-list_inputs", "goobers-io-read_input", "goobers-io-grep_input",
	}
}
