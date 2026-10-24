package schedcounters

import (
	"context"
	"errors"
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/remediationselect"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/providers"
)

// demandCounter sizes a due scheduled update-behind-pr fire to the unclaimed
// pull requests the remediation stages would select. It is the cloud's
// remediationDemandCounter: the selection is remediationselect's, the ledger and
// the journal read are the cloud's.
type demandCounter struct {
	open       Opener
	quota      *localscheduler.ProviderQuotaState
	claims     remediationselect.NamespaceLister
	journal    remediationselect.BranchOwnershipReader
	now        func() time.Time
	repo       providers.RepositoryRef
	gaggle     string
	base       string
	headPrefix string
}

var _ localscheduler.ProviderQuotaGuardedBacklogCounter = (*demandCounter)(nil)

// ProviderQuotaGuarded tells the scheduler this counter enforces the quota
// ledger itself.
func (c *demandCounter) ProviderQuotaGuarded() bool { return c.quota != nil }

func (c *demandCounter) EligibleCount(ctx context.Context) (int, error) {
	access, done := accountFor(ctx, c.quota)
	defer done()
	provider, err := c.open(ctx, c.repo, access)
	if err != nil {
		return 0, fmt.Errorf("open provider for remediation count of %s/%s: %w", c.repo.Owner, c.repo.Name, err)
	}
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: c.repo, Base: c.base, HeadPrefix: c.headPrefix,
	})
	if err != nil {
		return 0, err
	}
	prs, blockedDependents, err := remediationselect.FilterRemediationPullRequests(ctx, provider, c.repo, prs, nil)
	if err != nil {
		return 0, err
	}
	claims, err := remediationselect.PullRequestClaimListingContext(ctx, c.claims, c.gaggle, c.repo.Provider)
	if err != nil {
		return 0, err
	}
	now := c.now()
	available := make([]providers.PullRequestSummary, 0, len(prs))
	for _, pr := range prs {
		claimed, ownedByCurrentRun := remediationselect.PullRequestClaimStatusFor(claims, c.gaggle, c.repo.Provider, pr, "", now)
		if !claimed || ownedByCurrentRun {
			available = append(available, pr)
		}
	}
	prs, _, err = remediationselect.FilterLiveOriginPullRequests(ctx, c.journal, c.gaggle, "", available)
	if err != nil {
		return 0, err
	}
	baseTips := map[string]string{}
	candidates, _, err := remediationselect.SelectRemediationCandidates(prs, blockedDependents, func(pr providers.PullRequestSummary) (bool, error) {
		return remediationselect.PullRequestBehindLiveBase(ctx, provider, c.repo, pr, baseTips)
	})
	if err != nil {
		return 0, err
	}
	return len(candidates), nil
}

// scheduleDemand is the counter of a scheduled workflow whose first stage is the
// built-in update-behind-pr, built under the same conditions the local daemon
// builds its own: a schedule trigger, configured repositories, and not Azure
// DevOps (which has no update-behind remediation, so each due tick fires
// unsized there too).
func (c *Counters) scheduleDemand(gaggle apiv1.Gaggle, wf *apiv1.Workflow) (*demandCounter, error) {
	hasSchedule := false
	for _, trigger := range wf.Spec.Triggers {
		if trigger.Type == apiv1.TriggerSchedule {
			hasSchedule = true
			break
		}
	}
	base, headPrefix, ok := remediationCounterScope(wf, runtimewiring.BranchNamespacesByGaggle(c.deps.Set)[wf.Spec.Gaggle])
	if !hasSchedule || !ok || len(c.deps.Config.Repos) == 0 || counterProviderKind(c.deps, gaggle) == apiv1.ProviderADO {
		return nil, nil
	}
	if c.deps.Claims == nil || c.deps.Journal == nil {
		return nil, errors.New("schedule demand needs the cloud claims namespace and the journal's branch ownership; neither may be omitted")
	}
	repo, err := c.repoFor(gaggle.Spec.Project)
	if err != nil {
		return nil, err
	}
	now := c.deps.Now
	if now == nil {
		now = time.Now
	}
	return &demandCounter{
		open: c.deps.Open, quota: c.deps.Quota, claims: c.deps.Claims, journal: c.deps.Journal, now: now,
		repo: repo, gaggle: wf.Spec.Gaggle, base: base, headPrefix: headPrefix,
	}, nil
}

// counterProviderKind resolves the provider a demand counter talks to the way
// schedentries decides whether one is needed: the configured repository bound to
// the gaggle's project wins over the gaggle's own provider.
func counterProviderKind(deps Deps, gaggle apiv1.Gaggle) apiv1.Provider {
	provider := gaggle.Spec.Project.Provider
	project := gaggle.Spec.Project
	if configured, ok := runtimewiring.ConfiguredRepoForProject(deps.Config, project); ok && configured.Provider != "" {
		provider = apiv1.Provider(configured.Provider)
	} else {
		// An older gaggle that omits its provider is matched by repository identity.
		for _, configured := range deps.Config.Repos {
			if configured.Owner == project.Owner && configured.Project == project.Project && configured.Name == project.Name {
				if configured.Provider != "" {
					provider = apiv1.Provider(configured.Provider)
				}
				break
			}
		}
	}
	if provider == "" {
		provider = apiv1.ProviderGitHub
	}
	return provider
}

// remediationCounterScope recognises the built-in update-behind-pr selector and
// returns the base branch and run-branch prefix it targets.
func remediationCounterScope(wf *apiv1.Workflow, branchNamespace string) (base, headPrefix string, ok bool) {
	for _, task := range wf.Spec.Tasks {
		if task.Name != wf.Spec.Start || task.Run == nil || len(task.Run.Command) != 2 ||
			task.Run.Command[0] != "goobers" || task.Run.Command[1] != "update-behind-pr" {
			continue
		}
		base = task.Inputs["base"]
		if base == "" {
			base = "main"
		}
		headPrefix = task.Inputs["headPrefix"]
		if headPrefix == "" {
			headPrefix = providers.NormalizeBranchNamespace(branchNamespace)
		}
		return base, headPrefix, true
	}
	return "", "", false
}
