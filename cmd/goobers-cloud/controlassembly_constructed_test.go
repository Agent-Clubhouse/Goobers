package main

import "testing"

// TestEveryControlSeamHasAConstructor: production composition
// (composeControlPlanes) skips a seam without a constructor, so the assembly
// never completes and the role serves only its status handler. The handler
// tests fill the seams with fakes and cannot see that, which is how RunJournal
// and Resolve stayed dark. Every seam must name its constructor.
func TestEveryControlSeamHasAConstructor(t *testing.T) {
	for _, seam := range controlAssembly {
		if seam.construct == nil {
			t.Errorf("control seam %s (%s) has no constructor: production composition would skip it and the API would never serve", seam.field, seam.issue)
		}
	}
}
