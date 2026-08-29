package runnercap

import (
	"reflect"
	"testing"
)

// The privilege token must be author-declarable on BOTH sides (a runner's
// provides.capabilities and a stage's runsOn.capabilities), so it must pass
// the author grammar — and it must NOT fall into the derived-tag namespace,
// which the solver satisfies implicitly for a self runner. A token that were
// derived would let every self runner "provide" Windows admin without
// claiming it (#3619).
func TestWindowsAdminTokenIsAuthorDeclarableAndNotDerived(t *testing.T) {
	if !ValidToken(CapabilityWindowsAdmin) {
		t.Fatalf("ValidToken(%q) = false: the privilege token must be spellable in provides.capabilities and runsOn.capabilities", CapabilityWindowsAdmin)
	}
	if err := ValidateToken(CapabilityWindowsAdmin); err != nil {
		t.Fatalf("ValidateToken(%q) = %v, want nil", CapabilityWindowsAdmin, err)
	}
	if DerivedTag(CapabilityWindowsAdmin) {
		t.Fatalf("DerivedTag(%q) = true: the privilege token must be an ordinary exact-match claim, never self-implicit", CapabilityWindowsAdmin)
	}
}

func TestHasWindowsAdmin(t *testing.T) {
	cases := []struct {
		tokens []string
		want   bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"dotnet@8"}, false},
		{[]string{"privilege=windows-admin"}, true},
		{[]string{"dotnet@8", CapabilityWindowsAdmin, "harness:copilot"}, true},
		// Exact match only: no prefix, case, or family matching.
		{[]string{"privilege=windows-administrator"}, false},
		{[]string{"privilege=Windows-Admin"}, false},
		{[]string{"privilege"}, false},
	}
	for _, tc := range cases {
		if got := HasWindowsAdmin(tc.tokens); got != tc.want {
			t.Errorf("HasWindowsAdmin(%v) = %v, want %v", tc.tokens, got, tc.want)
		}
	}
}

// Every member of the closed effect list has a decided Windows answer
// (restrictions doc D4 as corrected by #3619): exactly tmp:ephemeral and
// env:default-deny are declarable, the other three are not, and an effect
// outside the list is declarable nowhere. Growing the closed list without
// deciding its Windows binding fails here.
func TestDeclarableOnWindowsCoversClosedList(t *testing.T) {
	want := map[Restriction]bool{
		RestrictionEnvDefaultDeny:   true,
		RestrictionTmpEphemeral:     true,
		RestrictionFSReadonly:       false,
		RestrictionNetworkAllowlist: false,
		RestrictionNetworkNone:      false,
	}
	known := KnownRestrictions()
	if len(known) != len(want) {
		t.Fatalf("closed list has %d entries, this test decides %d — decide the Windows binding of the new effect", len(known), len(want))
	}
	for _, r := range known {
		decided, ok := want[r]
		if !ok {
			t.Fatalf("closed-list effect %q has no decided Windows binding in this test", r)
		}
		if got := DeclarableOnWindows(r); got != decided {
			t.Errorf("DeclarableOnWindows(%q) = %v, want %v", r, got, decided)
		}
	}
	if DeclarableOnWindows("seccomp") {
		t.Error("DeclarableOnWindows(unknown effect) = true, want false")
	}
	got := WindowsDeclarableRestrictions()
	if wantList := []Restriction{RestrictionEnvDefaultDeny, RestrictionTmpEphemeral}; !reflect.DeepEqual(got, wantList) {
		t.Errorf("WindowsDeclarableRestrictions() = %v, want %v (stable closed-list order)", got, wantList)
	}
}
