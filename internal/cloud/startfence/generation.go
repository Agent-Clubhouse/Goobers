package startfence

// GenerationAdmission is the active configuration's engine eligibility guard.
// The control role supplies the guard of its one shared configuration applier.
type GenerationAdmission interface {
	Admit(digest, gaggle, workflow string) error
}
