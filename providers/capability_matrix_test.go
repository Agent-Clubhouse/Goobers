package providers

import "testing"

// TestBlessedTierGapsAreTracked is CONF-2's (#2075) CI gate: "Blessed-tier
// rule enforced in CI with an actionable failure message." A failure here
// means GitHub or ADO silently stopped declaring a capability the epic's V1
// acceptance gate needs, with no linked issue explaining why — exactly the
// undocumented-divergence class the design doc's §5 blessed-tier rule
// exists to catch.
func TestBlessedTierGapsAreTracked(t *testing.T) {
	for _, err := range ValidateBlessedTier() {
		t.Error(err)
	}
}

// TestKnownGapsAreWithinRequiredSetAndBlessedTier catches a knownGaps entry
// that no longer means anything: a capability outside
// WorkflowRequiredCapabilities() (where no issue is required) or a
// provider outside the blessed tier (Gitea owes no gap-issue
// justification, §5) would silently rot instead of ever being exercised by
// TestBlessedTierGapsAreTracked.
func TestKnownGapsAreWithinRequiredSetAndBlessedTier(t *testing.T) {
	required := WorkflowRequiredCapabilities()
	blessed := map[ProviderKind]bool{}
	for _, kind := range BlessedTierProviderKinds() {
		blessed[kind] = true
	}
	for kind, caps := range knownGaps {
		if !blessed[kind] {
			t.Errorf("knownGaps has an entry for %q, which is not a blessed-tier provider", kind)
		}
		for cap, issue := range caps {
			if !required.Has(cap) {
				t.Errorf("knownGaps[%q][%q] = %q, but %q is outside WorkflowRequiredCapabilities()", kind, cap, issue, cap)
			}
			if issue == "" {
				t.Errorf("knownGaps[%q][%q] has an empty issue reference", kind, cap)
			}
		}
	}
}

// TestBuildMatrixCoversEveryCapabilityAndProvider guards the matrix's own
// completeness: one cell per (capability, provider) pair, no silent gaps
// in the generator's double loop.
func TestBuildMatrixCoversEveryCapabilityAndProvider(t *testing.T) {
	cells := BuildMatrix()
	want := len(AllCapabilities()) * len(AllProviderKinds())
	if len(cells) != want {
		t.Fatalf("BuildMatrix() returned %d cells, want %d (%d capabilities x %d providers)",
			len(cells), want, len(AllCapabilities()), len(AllProviderKinds()))
	}

	seen := make(map[[2]string]bool, len(cells))
	for _, cell := range cells {
		key := [2]string{string(cell.Capability), string(cell.Provider)}
		if seen[key] {
			t.Errorf("duplicate matrix cell for capability %q provider %q", cell.Capability, cell.Provider)
		}
		seen[key] = true
	}
}

func TestGiteaSupportIsExperimentalWithPromotionCriteria(t *testing.T) {
	for _, support := range AllProviderSupport() {
		if support.Provider != ProviderGitea {
			continue
		}
		if support.Level != ProviderExperimental {
			t.Fatalf("Gitea support level = %q, want %q", support.Level, ProviderExperimental)
		}
		if support.PromotionCriteria == "" {
			t.Fatal("Gitea promotion criteria are empty")
		}
		return
	}
	t.Fatal("Gitea is missing from provider support declarations")
}

// TestGitHubIsFullyConformantOnRequiredSet pins the positive counterpart:
// the V0 workload provider must have zero gaps against
// WorkflowRequiredCapabilities() — if this ever fails, GitHub regressed a
// capability the epic's acceptance gate assumes always works.
func TestGitHubIsFullyConformantOnRequiredSet(t *testing.T) {
	declared, _ := CapabilitiesFor(ProviderGitHub)
	for cap := range WorkflowRequiredCapabilities() {
		cell := classify(ProviderGitHub, cap, declared.Has(cap))
		if cell.Status != StatusConformant {
			t.Errorf("GitHub capability %q status = %q, want %q", cap, cell.Status, StatusConformant)
		}
	}
}
