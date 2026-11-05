package journal

// ConfigGenerationAppliedAnnotation marks a daemon startup's committed catalog.
const ConfigGenerationAppliedAnnotation = "config.generation.applied"

// IsConfigGenerationAuthority identifies events that supersede prior catalog
// authority. A legacy config.reloaded event without a generation is a barrier:
// it must survive retention even when a clock rollback makes it look older
// than the modern catalog it supersedes.
func IsConfigGenerationAuthority(event Event) bool {
	return event.Type == EventConfigReloaded || event.Type == EventRunnerAnnotation && event.Runner["kind"] == ConfigGenerationAppliedAnnotation
}
