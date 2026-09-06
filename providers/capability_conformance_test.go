package providers

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// optionalCapabilityInterfaces maps each capability that is NOT in
// mandatoryCapabilities() to the Go interface that expresses it. This is
// the cross-check acceptance criterion from CONF-1 (#2074, design doc
// docs/design/provider-contract-conformance.md §3.2/§7 item 1): a provider
// cannot declare a capability it does not implement, and — for the blessed
// tier (GitHub, ADO) — cannot implement a capability's interface without
// declaring it either. Community adapters (Gitea) are held to the weaker
// bar the design doc states (§5): conformant on whatever they declare,
// nothing more required.
var optionalCapabilityInterfaces = map[Capability]reflect.Type{
	CapPRCompare:             reflect.TypeOf((*CommitComparer)(nil)).Elem(),
	CapPRReviewSubmit:        reflect.TypeOf((*PullRequestReviewSubmitter)(nil)).Elem(),
	CapPRReviewThreads:       reflect.TypeOf((*PullRequestReviewThreadProvider)(nil)).Elem(),
	CapPRReviewResolve:       reflect.TypeOf((*PullRequestReviewThreadMutator)(nil)).Elem(),
	CapPRMerge:               reflect.TypeOf((*PullRequestMerger)(nil)).Elem(),
	CapPRLandingDetectPolicy: reflect.TypeOf((*MergePolicyDetector)(nil)).Elem(),
	CapPRLandingEnqueue:      reflect.TypeOf((*PullRequestEnqueuer)(nil)).Elem(),
	CapPRLandingPoll:         reflect.TypeOf((*MergeQueuePoller)(nil)).Elem(),
	CapPRUpdateBranch:        reflect.TypeOf((*PullRequestBranchUpdater)(nil)).Elem(),
	CapBranchDelete:          reflect.TypeOf((*BranchDeleter)(nil)).Elem(),
	CapRepoPolicyRead:        reflect.TypeOf((*PolicyProvider)(nil)).Elem(),
	CapPRStatusPublish:       reflect.TypeOf((*PullRequestStatusPublisher)(nil)).Elem(),
	CapCICancel:              reflect.TypeOf((*PendingCheckCanceler)(nil)).Elem(),
	CapBacklogBlockers:       reflect.TypeOf((*WorkItemBlockerChecker)(nil)).Elem(),
}

// TestBlessedTierCapabilitiesCrossCheckInterfaceSatisfaction is CONF-1's
// acceptance criterion: "conformance test cross-checks declaration ⇔
// optional-interface satisfaction both directions" for GitHub and ADO, the
// blessed tier (§2 goals: "Both reach full conformance on the workflow-
// required capability set; they move in lockstep from here on").
func TestBlessedTierCapabilitiesCrossCheckInterfaceSatisfaction(t *testing.T) {
	blessed := []Provider{
		&GitHubProvider{},
		&ADOProvider{},
	}
	for _, p := range blessed {
		p := p
		t.Run(string(p.Kind()), func(t *testing.T) {
			declared := p.Capabilities()
			concrete := reflect.TypeOf(p)
			for cap, ifaceType := range optionalCapabilityInterfaces {
				implements := concrete.Implements(ifaceType)
				isDeclared := declared.Has(cap)
				if isDeclared && !implements {
					t.Errorf("provider %q declares capability %q but does not implement %s — cannot declare what isn't implemented", p.Kind(), cap, ifaceType)
				}
				if implements && !isDeclared {
					t.Errorf("provider %q implements %s (capability %q) but does not declare it — cannot implement what isn't declared", p.Kind(), ifaceType, cap)
				}
			}
		})
	}
}

// TestBlessedTierDeclaresLandingSurfaces pins CONF-3's third acceptance
// criterion literally (#2076, design doc §4): "ADO capability declaration
// updated to include the landing set" — GitHub and ADO both declare the
// full landing set as conformant, in lockstep per §2's blessed-tier goal.
// (CONF-1 (#2074) pinned the inverse — ADO honestly excluding this set
// pending this issue — which this test now supersedes.)
func TestBlessedTierDeclaresLandingSurfaces(t *testing.T) {
	required := []Capability{
		CapPRMerge,
		CapPRLandingDetectPolicy,
		CapPRLandingEnqueue,
		CapPRLandingPoll,
		CapBranchDelete,
		CapPRCompare,
	}
	blessed := []Provider{&GitHubProvider{}, &ADOProvider{}}
	for _, p := range blessed {
		declared := p.Capabilities()
		for _, cap := range required {
			if !declared.Has(cap) {
				t.Errorf("provider %q does not declare capability %q, want declared", p.Kind(), cap)
			}
		}
	}
}

// TestADOStillExcludesUnimplementedSurfaces guards the capabilities CONF-3
// did NOT touch — no ADO implementation exists for these, so a regression
// here would silently resurrect the #2059 fail-open class for a different
// capability.
func TestADOStillExcludesUnimplementedSurfaces(t *testing.T) {
	ado := (&ADOProvider{}).Capabilities()
	excluded := []Capability{
		CapPRReviewSubmit,
		CapPRReviewThreads,
		CapPRReviewResolve,
		CapRepoPolicyRead,
		CapPRUpdateBranch,
		CapCICancel,
	}
	for _, cap := range excluded {
		if ado.Has(cap) {
			t.Errorf("ADO declares capability %q, want excluded (no ADO implementation exists)", cap)
		}
	}
}

// TestCommunityAdapterCannotDeclareWhatItDoesNotImplement holds Gitea to
// the weaker community-adapter bar (§5): declared capabilities must be
// backed by a real interface implementation, but Gitea may implement an
// interface method (e.g. a typed-sentinel-error stub) without declaring it
// conformant — that direction is a blessed-tier-only requirement.
func TestCommunityAdapterCannotDeclareWhatItDoesNotImplement(t *testing.T) {
	gitea := &GiteaProvider{}
	declared := gitea.Capabilities()
	concrete := reflect.TypeOf(gitea)
	for cap, ifaceType := range optionalCapabilityInterfaces {
		if declared.Has(cap) && !concrete.Implements(ifaceType) {
			t.Errorf("gitea declares capability %q but does not implement %s", cap, ifaceType)
		}
	}
}

// TestEveryProviderDeclaresMandatoryCapabilities guards against a provider
// dropping a baseline capability it structurally must carry (the compiler
// already enforces the underlying method exists via RepoProvider/
// BacklogProvider/TriggerProvider) — a regression here means Capabilities()
// itself was miscomposed, not that the provider stopped implementing the
// method.
func TestEveryProviderDeclaresMandatoryCapabilities(t *testing.T) {
	all := []Provider{&GitHubProvider{}, &ADOProvider{}, &GiteaProvider{}}
	for _, p := range all {
		declared := p.Capabilities()
		for cap := range mandatoryCapabilities() {
			if !declared.Has(cap) {
				t.Errorf("provider %q does not declare mandatory capability %q", p.Kind(), cap)
			}
		}
	}
}

// TestDispatcherRefusesUndeclaredCapability is CONF-1's second acceptance
// criterion: "calling an undeclared capability returns ErrUnsupported from
// the shim; provider code is never entered". ADOProvider's zero value has
// no HTTP client configured, so if GetRepoPolicy reached the provider it
// would panic/error on a nil dependency rather than return this typed
// ErrUnsupported — proving the shim refuses before dispatch. repo.policy.read
// has no ADO implementation at all (unlike pr.merge, which CONF-3 (#2076)
// made conformant), so it stays a reliable "undeclared" example.
func TestDispatcherRefusesUndeclaredCapability(t *testing.T) {
	d := NewDispatcher(&ADOProvider{})
	_, err := d.GetRepoPolicy(context.Background(), RepoPolicyRequest{})
	var unsupported ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("GetRepoPolicy error = %v, want ErrUnsupported", err)
	}
	if unsupported.Provider != ProviderADO || unsupported.Capability != CapRepoPolicyRead {
		t.Fatalf("ErrUnsupported = %+v, want Provider=%q Capability=%q", unsupported, ProviderADO, CapRepoPolicyRead)
	}
}

// fakeCapableProvider is a minimal Provider that declares and implements
// exactly one optional capability, for exercising Dispatcher's happy path.
type fakeCapableProvider struct {
	Provider
	caps       CapabilitySet
	mergeCalls int
}

func (f *fakeCapableProvider) Kind() ProviderKind          { return ProviderGitHub }
func (f *fakeCapableProvider) Capabilities() CapabilitySet { return f.caps }
func (f *fakeCapableProvider) MergePullRequest(context.Context, MergePullRequestRequest) (MergePullRequestResult, error) {
	f.mergeCalls++
	return MergePullRequestResult{MergeSHA: "abc123"}, nil
}

// TestDispatcherCallsThroughWhenDeclared proves the shim is not merely a
// refusal path: a declared, implemented capability reaches the provider.
func TestDispatcherCallsThroughWhenDeclared(t *testing.T) {
	fake := &fakeCapableProvider{caps: NewCapabilitySet(CapPRMerge)}
	d := NewDispatcher(fake)
	result, err := d.MergePullRequest(context.Background(), MergePullRequestRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.MergeSHA != "abc123" {
		t.Fatalf("MergeSHA = %q, want abc123", result.MergeSHA)
	}
	if fake.mergeCalls != 1 {
		t.Fatalf("provider called %d times, want 1", fake.mergeCalls)
	}
}
