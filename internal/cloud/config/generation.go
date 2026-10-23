package config

// Generation returns the digest of the applied generation, or "" before the
// first successful apply. Triggers pin starts to it.
func (c *Configuration) Generation() string { return c.applier.Status().Applied }
