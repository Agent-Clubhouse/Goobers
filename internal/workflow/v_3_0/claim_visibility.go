package v30

func claimVisibilityProblems(mode string) []string {
	switch mode {
	case "", "local", "shared":
		return nil
	default:
		return []string{"spec.readiness.claimVisibility must be local or shared"}
	}
}
