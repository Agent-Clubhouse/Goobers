package providers

import (
	"strings"
	"testing"
)

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
		for cap, gap := range caps {
			if !required.Has(cap) {
				t.Errorf("knownGaps[%q][%q] = %+v, but %q is outside WorkflowRequiredCapabilities()", kind, cap, gap, cap)
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

// TestGapRegistryEntriesAreWellFormed is the offline half of #3058's gate:
// every knownGaps entry must be unambiguously one kind or the other — a
// tracked gap with an issue, or a permanent non-applicability with a
// rationale — so a forge difference can never masquerade as unfinished
// work.
func TestGapRegistryEntriesAreWellFormed(t *testing.T) {
	for _, err := range ValidateGapRegistry() {
		t.Error(err)
	}
}

// TestADOPullRequestAssigneeIsPermanentlyNotApplicable pins the concrete
// bug #3058 reports: ADO has no PR-assignee concept, so the cell must
// render as a permanent difference with a rationale, never as a gap
// linking a (now closed) tracking issue.
func TestADOPullRequestAssigneeIsPermanentlyNotApplicable(t *testing.T) {
	gap, ok := gapFor(ProviderADO, CapPRQueryAssignee)
	if !ok {
		t.Fatalf("knownGaps has no entry for ADO %q", CapPRQueryAssignee)
	}
	if gap.Kind != GapNotApplicable {
		t.Errorf("gap kind = %q, want %q", gap.Kind, GapNotApplicable)
	}
	if gap.Issue != "" {
		t.Errorf("gap issue = %q, want no issue reference for a permanent difference", gap.Issue)
	}
	if gap.Rationale == "" {
		t.Error("gap rationale is empty")
	}

	declared, _ := CapabilitiesFor(ProviderADO)
	cell := classify(ProviderADO, CapPRQueryAssignee, declared.Has(CapPRQueryAssignee))
	if cell.Status != StatusNotApplicable {
		t.Errorf("cell status = %q, want %q", cell.Status, StatusNotApplicable)
	}
	if cell.GapIssue != "" {
		t.Errorf("cell gap issue = %q, want empty", cell.GapIssue)
	}
	if cell.Rationale != gap.Rationale {
		t.Errorf("cell rationale = %q, want %q", cell.Rationale, gap.Rationale)
	}
}

func TestClassifyRendersEachGapKind(t *testing.T) {
	restore := swapKnownGaps(t, map[ProviderKind]map[Capability]CapabilityGap{
		ProviderADO: {
			CapBacklogBlockers: {Kind: GapTracked, Issue: "#3030"},
			CapPRQueryAssignee: {Kind: GapNotApplicable, Rationale: "no such concept"},
		},
	})
	defer restore()

	tests := []struct {
		name     string
		cap      Capability
		declared bool
		want     MatrixStatus
	}{
		{"tracked and undeclared", CapBacklogBlockers, false, StatusGap},
		{"tracked but declared", CapBacklogBlockers, true, StatusDeclaredGap},
		{"not applicable", CapPRQueryAssignee, false, StatusNotApplicable},
		{"unregistered and declared", CapPRMerge, true, StatusConformant},
		{"unregistered and undeclared", CapPRMerge, false, StatusNotDeclared},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(ProviderADO, tc.cap, tc.declared).Status; got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateGapRegistryRejectsMalformedEntries(t *testing.T) {
	tests := []struct {
		name string
		gap  CapabilityGap
		want string
	}{
		{"tracked without issue", CapabilityGap{Kind: GapTracked}, "not of the form"},
		{"tracked with prose issue", CapabilityGap{Kind: GapTracked, Issue: "see the epic"}, "not of the form"},
		{"tracked with rationale", CapabilityGap{Kind: GapTracked, Issue: "#3030", Rationale: "because"}, "explained by their issue"},
		{"not applicable without rationale", CapabilityGap{Kind: GapNotApplicable}, "carries no rationale"},
		{"not applicable with issue", CapabilityGap{Kind: GapNotApplicable, Issue: "#2178", Rationale: "because"}, "no fix to track"},
		{"unknown kind", CapabilityGap{Kind: "someday"}, "unknown gap kind"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restore := swapKnownGaps(t, map[ProviderKind]map[Capability]CapabilityGap{
				ProviderADO: {CapBacklogBlockers: tc.gap},
			})
			defer restore()

			errs := ValidateGapRegistry()
			if len(errs) == 0 {
				t.Fatalf("ValidateGapRegistry() accepted %+v", tc.gap)
			}
			if !strings.Contains(errs[0].Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", errs[0], tc.want)
			}
		})
	}
}

// TestValidateBlessedTierRejectsNotApplicableOnDeclaredCapability catches
// the contradiction the two-kind registry makes possible: a capability the
// provider genuinely implements recorded as permanently absent would hide
// working behavior from the matrix.
func TestValidateBlessedTierRejectsNotApplicableOnDeclaredCapability(t *testing.T) {
	gaps := map[ProviderKind]map[Capability]CapabilityGap{ProviderADO: {}}
	for cap, gap := range knownGaps[ProviderADO] {
		gaps[ProviderADO][cap] = gap
	}
	gaps[ProviderADO][CapPRMerge] = CapabilityGap{Kind: GapNotApplicable, Rationale: "not really"}
	restore := swapKnownGaps(t, gaps)
	defer restore()

	errs := ValidateBlessedTier()
	if len(errs) != 1 {
		t.Fatalf("ValidateBlessedTier() = %v, want exactly the contradiction error", errs)
	}
	if !strings.Contains(errs[0].Error(), "permanently not applicable") {
		t.Errorf("error = %q, want it to name the contradiction", errs[0])
	}
}

// swapKnownGaps installs a registry for one test and returns the restore.
func swapKnownGaps(t *testing.T, replacement map[ProviderKind]map[Capability]CapabilityGap) func() {
	t.Helper()
	original := knownGaps
	knownGaps = replacement
	return func() { knownGaps = original }
}
