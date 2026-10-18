package claimrelease

// KnownDispositions exposes the closed vocabulary to the external schema
// drift test, which cannot live in this package (internal/journal imports it).
func KnownDispositions() []Disposition {
	dispositions := make([]Disposition, 0, len(known))
	for d := range known {
		dispositions = append(dispositions, d)
	}
	return dispositions
}
