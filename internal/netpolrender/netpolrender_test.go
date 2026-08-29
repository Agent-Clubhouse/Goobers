package netpolrender

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/runnercap"
)

// fixtureInput covers the three class shapes: a network:none class, a
// network:allowlist class, and a class with no network:* effect.
func fixtureInput() Input {
	return Input{
		Runners: []Runner{
			{Name: "locked", Restrictions: []string{"network:none", "fs:readonly-except-workspace"}},
			{Name: "ci-linux", Restrictions: []string{"network:allowlist", "fs:readonly-except-workspace", "tmp:ephemeral"}},
			// Same set as ci-linux in a different order — must collapse into
			// the same class.
			{Name: "ci-linux-b", Restrictions: []string{"tmp:ephemeral", "network:allowlist", "fs:readonly-except-workspace"}},
			{Name: "open", Restrictions: []string{"tmp:ephemeral"}},
		},
		Allowlist: []AllowlistGroup{
			{
				Name: "github-provider", Kind: GroupKindProvider,
				Source: "https://api.github.com/meta", SourceSHA256: strings.Repeat("ab", 32),
				CIDRs: []string{"140.82.112.0/20"},
			},
			{
				Name: "copilot-model", Kind: GroupKindModel,
				Source: "https://api.github.com/meta", SourceSHA256: strings.Repeat("ab", 32),
				CIDRs: []string{"140.82.113.21/32", "140.82.113.22/32"},
			},
		},
	}
}

// parseRenderedPolicies re-parses the rendered FILES — the actual product
// output, not the in-memory objects — so the assertions observe what an
// adopter's kubectl would.
func parseRenderedPolicies(t *testing.T, result *Result) map[string]*networkingv1.NetworkPolicy {
	t.Helper()
	policies := make(map[string]*networkingv1.NetworkPolicy)
	for _, file := range result.Files {
		if file.Name == "kustomization.yaml" {
			continue
		}
		var policy networkingv1.NetworkPolicy
		if err := yaml.Unmarshal(file.Content, &policy); err != nil {
			t.Fatalf("parse %s: %v", file.Name, err)
		}
		value := policy.Spec.PodSelector.MatchLabels[runnercap.LabelRunnerClass]
		if value == "" {
			t.Fatalf("%s: policy selects no %s label", file.Name, runnercap.LabelRunnerClass)
		}
		policies[value] = &policy
	}
	return policies
}

// TestRenderPeerFormIsSingleElementAND is the security-critical assertion
// (dispatcher §2a, #3567): every cross-namespace grant — the blob-endpoint
// row and the DNS row — combines namespaceSelector AND podSelector in ONE
// to-element. It PARSES the composed peer rather than grepping: the OR form
// (same selectors as separate peers — the whole-namespace egress-proxy
// bypass) and the podSelector-only form (matches nothing; the stage hangs at
// materialize) each differ from the correct form by two characters of YAML
// indentation.
func TestRenderPeerFormIsSingleElementAND(t *testing.T) {
	result, err := Render(fixtureInput())
	if err != nil {
		t.Fatal(err)
	}
	policies := parseRenderedPolicies(t, result)
	if len(policies) != 3 {
		t.Fatalf("got %d classes, want 3", len(policies))
	}

	for value, policy := range policies {
		var selectorRules int
		for _, rule := range policy.Spec.Egress {
			var hasSelectorPeer bool
			for _, peer := range rule.To {
				if peer.IPBlock != nil {
					if peer.NamespaceSelector != nil || peer.PodSelector != nil {
						t.Errorf("class %s: a peer mixes ipBlock with selectors", value)
					}
					continue
				}
				hasSelectorPeer = true
				// THE assertion: a selector peer must carry BOTH halves in
				// this one element. A podSelector-only peer selects the
				// policy's own namespace and grants nothing across
				// namespaces; a namespaceSelector-only peer grants the whole
				// namespace.
				if peer.NamespaceSelector == nil || peer.PodSelector == nil {
					t.Errorf("class %s: cross-namespace peer is not the single-element AND form: %+v", value, peer)
				}
				if peer.PodSelector != nil && len(peer.PodSelector.MatchLabels) == 0 && len(peer.PodSelector.MatchExpressions) == 0 {
					t.Errorf("class %s: peer podSelector is empty — that is the namespace-wide grant in disguise", value)
				}
			}
			if hasSelectorPeer {
				selectorRules++
				if len(rule.To) != 1 {
					t.Errorf("class %s: a selector rule has %d peers; the AND-form grant is exactly one composed peer per rule",
						value, len(rule.To))
				}
			}
		}
		if selectorRules != 2 {
			t.Errorf("class %s: got %d selector rules, want 2 (DNS + blob endpoint)", value, selectorRules)
		}
	}
}

// TestRenderEveryClassCarriesBlobAndDNS: decision 012 — the blob row is every
// class's own data path, restricted INCLUDED; without it a network:none stage
// hangs at materialize, indistinguishable from a missing row.
func TestRenderEveryClassCarriesBlobAndDNS(t *testing.T) {
	result, err := Render(fixtureInput())
	if err != nil {
		t.Fatal(err)
	}
	blob := DefaultBlobEndpoint()
	for value, policy := range parseRenderedPolicies(t, result) {
		var hasDNS, hasBlob bool
		for _, rule := range policy.Spec.Egress {
			for _, peer := range rule.To {
				if peer.NamespaceSelector == nil || peer.PodSelector == nil {
					continue
				}
				switch peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] {
				case "kube-system":
					hasDNS = true
				case blob.Namespace:
					if peer.PodSelector.MatchLabels["app.kubernetes.io/name"] == blob.PodLabels["app.kubernetes.io/name"] {
						hasBlob = true
					}
				}
			}
		}
		if !hasDNS {
			t.Errorf("class %s carries no DNS egress row", value)
		}
		if !hasBlob {
			t.Errorf("class %s carries no blob-endpoint egress row (decision 012: restricted included)", value)
		}
	}
}

// TestRenderNetworkNoneClassGrantsNoCIDRs: the deny-all class gets ONLY DNS
// and the blob data path.
func TestRenderNetworkNoneClassGrantsNoCIDRs(t *testing.T) {
	result, err := Render(fixtureInput())
	if err != nil {
		t.Fatal(err)
	}
	noneValue := runnercap.RunnerClassValue([]string{"network:none", "fs:readonly-except-workspace"})
	policy := parseRenderedPolicies(t, result)[noneValue]
	if policy == nil {
		t.Fatalf("no policy rendered for the network:none class %s", noneValue)
	}
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				t.Errorf("network:none class grants CIDR %s", peer.IPBlock.CIDR)
			}
		}
	}
	if got := len(policy.Spec.Egress); got != 2 {
		t.Errorf("network:none class has %d egress rules, want exactly 2 (DNS + blob)", got)
	}
}

// TestRenderAnnotationPreimageRoundTrips is the issue #3568 diagnosability
// acceptance: every rendered policy's restriction annotation, decoded and
// re-derived through runnercap.RunnerClassValue, must equal the policy's own
// selector value — the annotation is emitted from the SAME canonical input
// the selector was hashed from, never hand-added.
func TestRenderAnnotationPreimageRoundTrips(t *testing.T) {
	input := fixtureInput()
	// Include a class that lands on the opaque rc-<hash> fallback — the case
	// the annotation exists for: the label value alone is undecodable.
	input.Runners = append(input.Runners, Runner{Name: "future", Restrictions: []string{"future:effect", "network:none"}})
	result, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	policies := parseRenderedPolicies(t, result)
	var sawHashFallback bool
	for value, policy := range policies {
		preimage, ok := policy.Annotations[runnercap.AnnotationRunnerClassRestrictions]
		if !ok {
			t.Errorf("class %s: policy carries no %s annotation", value, runnercap.AnnotationRunnerClassRestrictions)
			continue
		}
		if derived := runnercap.RunnerClassValue(runnercap.ParseRunnerClassPreimage(preimage)); derived != value {
			t.Errorf("class %s: annotation preimage %q derives %q — the annotation does not describe the selector",
				value, preimage, derived)
		}
		if strings.HasPrefix(value, "rc-") {
			sawHashFallback = true
		}
		if _, isLabel := policy.Labels[runnercap.AnnotationRunnerClassRestrictions]; isLabel {
			t.Errorf("class %s: the restrictions preimage is a LABEL — it must never become a selector (decision 015)", value)
		}
	}
	if !sawHashFallback {
		t.Fatal("fixture produced no rc-<hash> class; the round-trip did not cover the opaque case")
	}
}

// TestRenderSelectorMatchesDispatcherStamp is the decision-015 round-trip at
// the render boundary: the selector value is produced by the same function
// the dispatcher stamps with, exercised — not just documented.
func TestRenderSelectorMatchesDispatcherStamp(t *testing.T) {
	input := fixtureInput()
	result, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	policies := parseRenderedPolicies(t, result)
	for _, runner := range input.Runners {
		stamped := runnercap.RunnerClassValue(runner.Restrictions)
		policy, ok := policies[stamped]
		if !ok {
			t.Errorf("runner %s: dispatcher would stamp %s=%q but no rendered policy selects it",
				runner.Name, runnercap.LabelRunnerClass, stamped)
			continue
		}
		if got := policy.Spec.PodSelector.MatchLabels[runnercap.LabelRole]; got != runnercap.RoleStage {
			t.Errorf("class %s: policy selects role %q, dispatcher stamps %q", stamped, got, runnercap.RoleStage)
		}
	}
}

func TestRenderZeroDeclarationInvariance(t *testing.T) {
	result, err := Render(Input{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 0 || len(result.Classes) != 0 {
		t.Fatalf("empty inventory rendered %d files, %d classes; want nothing", len(result.Files), len(result.Classes))
	}
}

func TestRenderRefusesPlaceholders(t *testing.T) {
	cases := []struct {
		name    string
		cidr    string
		wantErr string
	}{
		{"documentation-testnet2", "198.51.100.0/24", "documentation range"},
		{"documentation-testnet1-subrange", "192.0.2.128/25", "documentation range"},
		{"documentation-ipv6", "2001:db8:1::/48", "documentation range"},
		{"change-me-literal", "CHANGE-ME", "CHANGE-ME"},
		{"host-bits", "140.82.112.5/20", "host bits"},
		{"garbage", "not-a-cidr", "does not parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := fixtureInput()
			input.Allowlist[0].CIDRs = []string{tc.cidr}
			_, err := Render(input)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Render with CIDR %q: err = %v, want mention of %q", tc.cidr, err, tc.wantErr)
			}
		})
	}
}

// A class needing a destination set with NO configured allowlist fails —
// the render never emits a stub that looks deployable.
func TestRenderRefusesMissingAllowlist(t *testing.T) {
	input := fixtureInput()
	input.Allowlist = nil
	_, err := Render(input)
	if err == nil || !strings.Contains(err.Error(), "no egress.allowlist") {
		t.Fatalf("err = %v, want missing-allowlist refusal", err)
	}

	// All-network:none inventories need no CIDRs and must render.
	onlyNone := Input{Runners: []Runner{{Name: "locked", Restrictions: []string{"network:none"}}}}
	result, err := Render(onlyNone)
	if err != nil {
		t.Fatalf("all-none inventory should render without an allowlist: %v", err)
	}
	if len(result.Classes) != 1 {
		t.Fatalf("got %d classes, want 1", len(result.Classes))
	}
}

func TestRenderDeterministicAndDeduplicated(t *testing.T) {
	first, err := Render(fixtureInput())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Render(fixtureInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Files) != len(second.Files) {
		t.Fatalf("file counts differ: %d vs %d", len(first.Files), len(second.Files))
	}
	for i := range first.Files {
		if first.Files[i].Name != second.Files[i].Name || !bytes.Equal(first.Files[i].Content, second.Files[i].Content) {
			t.Errorf("render is not deterministic at %s", first.Files[i].Name)
		}
	}
	// ci-linux and ci-linux-b share one restriction SET → one class.
	for _, class := range first.Classes {
		if contains(class.Runners, "ci-linux") {
			if !contains(class.Runners, "ci-linux-b") {
				t.Errorf("same restriction set split into two classes: %v", class.Runners)
			}
		}
	}
	if last := first.Files[len(first.Files)-1]; last.Name != "kustomization.yaml" {
		t.Errorf("last file is %s, want kustomization.yaml", last.Name)
	} else {
		for _, class := range first.Classes {
			if !strings.Contains(string(last.Content), classFileName(class.Value)) {
				t.Errorf("kustomization does not list %s", classFileName(class.Value))
			}
		}
	}
}

// TestRenderInlineProvenanceMarkers: the rendered CIDR-carrying files carry
// the upstream sha256 inline (must-carry control 1's marker half).
func TestRenderInlineProvenanceMarkers(t *testing.T) {
	result, err := Render(fixtureInput())
	if err != nil {
		t.Fatal(err)
	}
	allowValue := runnercap.RunnerClassValue([]string{"network:allowlist", "fs:readonly-except-workspace", "tmp:ephemeral"})
	var found bool
	for _, file := range result.Files {
		if file.Name != classFileName(allowValue) {
			continue
		}
		found = true
		if !strings.Contains(string(file.Content), "sha256:"+strings.Repeat("ab", 32)) {
			t.Errorf("%s carries no inline provenance sha256 marker", file.Name)
		}
	}
	if !found {
		t.Fatalf("no rendered file for the allowlist class %s", allowValue)
	}
}

// --- coverage math ---

// TestCoverageIsMeasuredInAddresses pins the unit lesson: few blocks can be
// nearly all the addresses. Three aggregates + eleven /32s = 14 blocks but
// 10,251 addresses; the aggregates alone are 10,240 of them.
func TestCoverageIsMeasuredInAddresses(t *testing.T) {
	model := AllowlistGroup{Name: "model", Kind: GroupKindModel, CIDRs: []string{
		"10.0.0.0/20", // 4096
		"10.1.0.0/20", // 4096
		"10.2.0.0/21", // 2048
		"10.9.0.1/32", "10.9.0.2/32", "10.9.0.3/32", "10.9.0.4/32", "10.9.0.5/32", "10.9.0.6/32",
		"10.9.0.7/32", "10.9.0.8/32", "10.9.0.9/32", "10.9.0.10/32", "10.9.0.11/32", // 11
	}}
	classes := Classes([]Runner{
		{Name: "ci", Restrictions: []string{"network:allowlist"}},
		{Name: "locked", Restrictions: []string{"network:none"}},
	})
	coverage, err := ModelEndpointCoverage(classes, []AllowlistGroup{model})
	if err != nil {
		t.Fatal(err)
	}
	allowValue := runnercap.RunnerClassValue([]string{"network:allowlist"})
	noneValue := runnercap.RunnerClassValue([]string{"network:none"})
	if got := coverage[allowValue]; got.Cmp(big.NewInt(10251)) != 0 {
		t.Errorf("allowlist class coverage = %s addresses, want 10251 (14 blocks would have said almost nothing)", got)
	}
	if got := coverage[noneValue]; got.Sign() != 0 {
		t.Errorf("network:none class coverage = %s, want 0", got)
	}
}

// Overlapping and duplicate CIDRs must not double-count.
func TestCoverageDeduplicatesOverlap(t *testing.T) {
	groups := []AllowlistGroup{
		{Name: "model", Kind: GroupKindModel, CIDRs: []string{"10.0.0.0/24", "10.0.0.0/25"}},
		{Name: "provider", Kind: GroupKindProvider, CIDRs: []string{"10.0.0.0/16"}},
	}
	classes := Classes([]Runner{{Name: "ci", Restrictions: []string{"network:allowlist"}}})
	coverage, err := ModelEndpointCoverage(classes, groups)
	if err != nil {
		t.Fatal(err)
	}
	value := runnercap.RunnerClassValue([]string{"network:allowlist"})
	if got := coverage[value]; got.Cmp(big.NewInt(256)) != 0 {
		t.Errorf("coverage = %s, want 256 (the /24 once — no double counting across overlap)", got)
	}
}

func TestCoverageCountsIPv6(t *testing.T) {
	groups := []AllowlistGroup{{Name: "model", Kind: GroupKindModel, CIDRs: []string{"2603:1030::/126"}}}
	classes := Classes([]Runner{{Name: "ci", Restrictions: []string{"network:allowlist"}}})
	coverage, err := ModelEndpointCoverage(classes, groups)
	if err != nil {
		t.Fatal(err)
	}
	value := runnercap.RunnerClassValue([]string{"network:allowlist"})
	if got := coverage[value]; got.Cmp(big.NewInt(4)) != 0 {
		t.Errorf("IPv6 /126 coverage = %s, want 4", got)
	}
}

// --- baseline ratchet ---

func ratchetFixture(t *testing.T) ([]Class, map[string]*big.Int) {
	t.Helper()
	classes := Classes([]Runner{{Name: "ci", Restrictions: []string{"network:allowlist"}}})
	return classes, map[string]*big.Int{classes[0].Value: big.NewInt(100)}
}

func TestBaselineFailsOnRiseOnly(t *testing.T) {
	classes, coverage := ratchetFixture(t)
	baseline := NewBaseline(classes, coverage)

	// Equal: passes.
	if _, err := CheckBaseline(baseline, classes, coverage); err != nil {
		t.Fatalf("equal coverage failed the ratchet: %v", err)
	}
	// Rise: fails.
	risen := map[string]*big.Int{classes[0].Value: big.NewInt(101)}
	if _, err := CheckBaseline(baseline, classes, risen); err == nil || !strings.Contains(err.Error(), "ROSE") {
		t.Fatalf("risen coverage: err = %v, want rise failure", err)
	}
	// Drop: passes with a note.
	dropped := map[string]*big.Int{classes[0].Value: big.NewInt(99)}
	notes, err := CheckBaseline(baseline, classes, dropped)
	if err != nil {
		t.Fatalf("dropped coverage failed the ratchet: %v", err)
	}
	if len(notes) == 0 {
		t.Error("dropped coverage produced no re-freeze note")
	}
}

// An unfrozen class must FAIL, not silently pass — the silent-toward-passing
// shape is the defect class both must-carry controls exist to close.
func TestBaselineRefusesUnfrozenClass(t *testing.T) {
	classes, coverage := ratchetFixture(t)
	empty := Baseline{Unit: BaselineUnit, Classes: map[string]BaselineEntry{}}
	if _, err := CheckBaseline(empty, classes, coverage); err == nil || !strings.Contains(err.Error(), "no frozen baseline entry") {
		t.Fatalf("err = %v, want unfrozen-class failure", err)
	}
}

func TestBaselineStaleEntryNotes(t *testing.T) {
	classes, coverage := ratchetFixture(t)
	baseline := NewBaseline(classes, coverage)
	baseline.Classes["gone"] = BaselineEntry{Restrictions: "network:none", ModelEndpointAddresses: "0"}
	notes, err := CheckBaseline(baseline, classes, coverage)
	if err != nil {
		t.Fatal(err)
	}
	var noted bool
	for _, note := range notes {
		if strings.Contains(note, "gone") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("stale baseline entry not noted: %v", notes)
	}
}

// The unit declaration is load-bearing: any other unit is refused, never
// reinterpreted.
func TestBaselineRefusesNonAddressUnits(t *testing.T) {
	raw := []byte(`{"unit":"cidr-blocks","classes":{}}`)
	if _, err := ParseBaseline(raw); err == nil || !strings.Contains(err.Error(), "addresses") {
		t.Fatalf("err = %v, want unit refusal", err)
	}
}

func TestBaselineRoundTrips(t *testing.T) {
	classes, coverage := ratchetFixture(t)
	baseline := NewBaseline(classes, coverage)
	raw, err := MarshalBaseline(baseline)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseBaseline(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckBaseline(parsed, classes, coverage); err != nil {
		t.Fatalf("round-tripped baseline failed its own coverage: %v", err)
	}
}

// --- provenance drift ---

// TestCheckProvenanceValidatesEveryMarker is the first-only-defect
// regression: the FIRST group's marker is current and the SECOND's is stale.
// A first-only check reports clean — the silent-narrowing defect fixed in
// Goobernetes-Infra@65a3d83/821a267 — so this test fails it.
func TestCheckProvenanceValidatesEveryMarker(t *testing.T) {
	body := []byte(`{"api":["140.82.112.0/20"]}`)
	liveSum := sha256Hex(body)
	groups := []AllowlistGroup{
		{Name: "first-current", Source: "https://api.github.com/meta", SourceSHA256: liveSum, CIDRs: []string{"140.82.112.0/20"}},
		{Name: "second-stale", Source: "https://api.github.com/meta", SourceSHA256: strings.Repeat("00", 32), CIDRs: []string{"140.82.112.0/20"}},
	}
	var fetches int
	fetch := func(ctx context.Context, url string) ([]byte, error) {
		fetches++
		return body, nil
	}
	mismatches, unverifiable := CheckProvenance(context.Background(), fetch, groups)
	if len(unverifiable) != 0 {
		t.Fatalf("unverifiable = %v, want none", unverifiable)
	}
	if len(mismatches) != 1 || mismatches[0].Group != "second-stale" {
		t.Fatalf("mismatches = %+v, want exactly the second group — a first-only check misses it", mismatches)
	}
	if fetches != 1 {
		t.Errorf("fetched %d times for one distinct URL, want 1 (every MARKER is validated, not every fetch repeated)", fetches)
	}

	// Both stale: both named.
	groups[0].SourceSHA256 = strings.Repeat("11", 32)
	mismatches, _ = CheckProvenance(context.Background(), fetch, groups)
	if len(mismatches) != 2 {
		t.Fatalf("both-stale mismatches = %+v, want both named", mismatches)
	}
}

func TestCheckProvenanceReportsFetchFailures(t *testing.T) {
	groups := []AllowlistGroup{
		{Name: "unreachable", Source: "https://api.github.com/meta", SourceSHA256: strings.Repeat("ab", 32)},
		{Name: "local-only", CIDRs: []string{"10.0.0.0/8"}},
	}
	fetch := func(ctx context.Context, url string) ([]byte, error) {
		return nil, errors.New("no route to host")
	}
	mismatches, unverifiable := CheckProvenance(context.Background(), fetch, groups)
	if len(mismatches) != 1 || mismatches[0].FetchErr == nil {
		t.Fatalf("mismatches = %+v, want one fetch failure", mismatches)
	}
	if len(unverifiable) != 1 || unverifiable[0] != "local-only" {
		t.Fatalf("unverifiable = %v, want the sourceless group named, never silently skipped", unverifiable)
	}
}

func sha256Hex(body []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(body))
}
