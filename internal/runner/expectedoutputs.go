package runner

// dslVersion is the pinned DSL version of the run's workflow, or "" when the
// input carries no compiled machine.
func (in StartInput) dslVersion() string {
	if in.Machine == nil {
		return ""
	}
	return in.Machine.Def.DSLVersion
}
