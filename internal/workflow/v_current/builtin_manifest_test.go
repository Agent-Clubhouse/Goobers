package vcurrent

import (
	"testing"

	"github.com/goobers/goobers/internal/providerstage"
)

// The admission call sites (admissionProblems, claimLedgerPlacementProblems,
// overPrivilegeWarnings) resolve built-in requirements through
// builtinManifest; this pins the view to this interpreter's own DSL version
// so a manifest change gated to a later DSL version can never surface here
// (#3504, the ed11ae81 class).
func TestBuiltinManifestResolvesThisInterpretersVersion(t *testing.T) {
	if builtinManifest != providerstage.ForVersion(DSLVersion) {
		t.Fatalf("builtinManifest is not the DSL %s view of the provider-stage manifest", DSLVersion)
	}
}
