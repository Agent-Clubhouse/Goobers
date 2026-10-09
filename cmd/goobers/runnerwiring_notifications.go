package main

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/escalationnotify"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/providerconfig"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

// newEscalationPoster constructs the provider the escalation notifier posts
// through — a package var so tests substitute a fake without a real GitHub
// client (mirrors newPRPoller).
var newEscalationPoster = func(token string) gate.Commenter { return providers.NewGitHubProvider(token) }

// escalationCommenter is the gate.Commenter the runner posts escalation
// comments through (#312). Like buildCIPollExecutor it resolves the org-repo
// token per call — honoring credentials.Resolver's re-read-on-resolve rotation
// contract rather than capturing a token once at daemon startup — registers it
// for scrubbing, then posts through a freshly-authenticated provider.
//
// On Azure DevOps there is no static repo token to resolve (azure-cli auth
// shells out to `az`), so the ADO branch builds a provider straight from
// instance config (adoauth) and routes the work-item mutation to the backlog
// project the PBI lives in — mirroring the provider-chain stages. Without this
// every ADO run's failure/park/escalation handler no-ops (token ref not found),
// leaking the goobers/status:claimed marker and never applying needs-human.
type escalationCommenter struct {
	resolver           credentials.Resolver
	reg                runner.SecretRegistrar
	layout             instance.Layout
	needsHumanAssignee string
}

func (c *escalationCommenter) configureAttribution(ctx context.Context, provider any) error {
	configurer, ok := provider.(providers.AttributionConfigurer)
	if !ok {
		return nil
	}
	attribution, ok := providers.AttributionFromContext(ctx)
	if !ok {
		return fmt.Errorf("refusing daemon-authored provider write without run attribution")
	}
	if attribution.Instance == "" && strings.TrimSpace(c.layout.Root) != "" {
		attribution.Instance = filepath.Base(filepath.Clean(c.layout.Root))
	}
	if attribution.Gaggle == "" {
		attribution.Gaggle = c.layout.Gaggle()
	}
	if attribution.Task == "" {
		attribution.Task = "runner"
	}
	if attribution.Goober == "" {
		attribution.Goober = "runner"
	}
	configurer.SetAttribution(attribution)
	return nil
}

func (c *escalationCommenter) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	// PR remediation uses pr/<number> as its internal claim key; provider work
	// item endpoints use the shared bare issue/PR number.
	req.ID = blockedLookupID(req.ID)
	req = withNeedsHumanAssignee(req, c.needsHumanAssignee)
	if req.Repository.Provider == providers.ProviderADO {
		backlog := backlogRepoRefForGaggle(c.layout, req.Repository)
		if !providerconfig.BacklogOnOtherProvider(req.Repository, backlog) {
			return c.updateADOWorkItem(ctx, req, backlog)
		}
		// A backlog on another provider (topology (b)): the item lives there,
		// reached through that repository's configured credential below.
		req.Repository = backlog
	}
	if req.Repository.Provider == providers.ProviderGitea {
		// Gitea authenticates with a static token like GitHub (resolved per call
		// through the rotation-aware resolver), but the mutation must reach the
		// self-hosted forge — newGiteaProviderForStage resolves its BaseURL from
		// instance config. The claim marker is the plain LabelClaimed (as GitHub),
		// so no ADO status-label rewrite is needed, and backlogRepoRefForGaggle is
		// a no-op for gitea (code repo and backlog coincide).
		ref := req.Repository.Owner + "/" + req.Repository.Name
		token, err := c.resolver.Resolve(ctx, ref)
		if err != nil {
			return providers.WorkItem{}, fmt.Errorf("resolve escalation-comment token for %s: %w", ref, err)
		}
		c.reg.Register([]byte(token))
		provider, err := newGiteaProviderForStage(c.layout.Root, req.Repository, token)
		if err != nil {
			return providers.WorkItem{}, fmt.Errorf("build gitea escalation provider for %s: %w", ref, err)
		}
		if err := c.configureAttribution(ctx, provider); err != nil {
			return providers.WorkItem{}, err
		}
		return provider.UpdateWorkItem(ctx, req)
	}
	ref := req.Repository.Owner + "/" + req.Repository.Name
	token, err := c.resolver.Resolve(ctx, ref)
	if err != nil {
		return providers.WorkItem{}, fmt.Errorf("resolve escalation-comment token for %s: %w", ref, err)
	}
	c.reg.Register([]byte(token))
	provider := newEscalationPoster(token)
	if err := c.configureAttribution(ctx, provider); err != nil {
		return providers.WorkItem{}, err
	}
	return provider.UpdateWorkItem(ctx, req)
}

// updateADOWorkItem is UpdateWorkItem for an item in an Azure DevOps backlog:
// the provider authenticates as the routed code repository's configured auth
// and addresses the backlog project.
func (c *escalationCommenter) updateADOWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest, backlog providers.RepositoryRef) (providers.WorkItem, error) {
	provider, err := newConfiguredADOProvider(c.layout.Root, req.Repository)
	if err != nil {
		return providers.WorkItem{}, fmt.Errorf("build ADO escalation provider for %s/%s: %w", req.Repository.Owner, req.Repository.Name, err)
	}
	if err := c.configureAttribution(ctx, provider); err != nil {
		return providers.WorkItem{}, err
	}
	req.Repository = backlog
	return provider.UpdateWorkItem(ctx, req)
}

func (c *escalationCommenter) ListComments(ctx context.Context, repository providers.RepositoryRef, itemID string) ([]providers.Comment, error) {
	itemID = blockedLookupID(itemID)
	if repository.Provider == providers.ProviderADO {
		backlog := backlogRepoRefForGaggle(c.layout, repository)
		if !providerconfig.BacklogOnOtherProvider(repository, backlog) {
			provider, err := newConfiguredADOProvider(c.layout.Root, repository)
			if err != nil {
				return nil, fmt.Errorf("build ADO escalation provider for %s/%s: %w", repository.Owner, repository.Name, err)
			}
			return provider.ListComments(ctx, backlog, itemID)
		}
		repository = backlog
	}
	ref := repository.Owner + "/" + repository.Name
	token, err := c.resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve escalation-comment token for %s: %w", ref, err)
	}
	c.reg.Register([]byte(token))
	if repository.Provider == providers.ProviderGitea {
		provider, err := newGiteaProviderForStage(c.layout.Root, repository, token)
		if err != nil {
			return nil, fmt.Errorf("build gitea escalation provider for %s: %w", ref, err)
		}
		return provider.ListComments(ctx, repository, itemID)
	}
	provider := newEscalationPoster(token)
	return provider.ListComments(ctx, repository, itemID)
}

// GetWorkItem implements gate.WorkItemReader so the failure-streak comment
// names the park label the item actually carries (#5430). It routes exactly
// like ListComments, except that an ADO pull request's labels are read from
// the PR on its code repository: its number is not a Boards work-item id.
func (c *escalationCommenter) GetWorkItem(ctx context.Context, repository providers.RepositoryRef, itemID string) (providers.WorkItem, error) {
	if repository.Provider == providers.ProviderADO && strings.HasPrefix(itemID, pullRequestClaimPrefix) {
		provider, err := newConfiguredADOProvider(c.layout.Root, repository)
		if err != nil {
			return providers.WorkItem{}, fmt.Errorf("build ADO escalation provider for %s/%s: %w", repository.Owner, repository.Name, err)
		}
		pullID := blockedLookupID(itemID)
		labels, err := provider.PullRequestLabelNames(ctx, repository, pullID)
		if err != nil {
			return providers.WorkItem{}, err
		}
		return providers.WorkItem{ID: pullID, Labels: labels}, nil
	}
	itemID = blockedLookupID(itemID)
	if repository.Provider == providers.ProviderADO {
		backlog := backlogRepoRefForGaggle(c.layout, repository)
		if !providerconfig.BacklogOnOtherProvider(repository, backlog) {
			provider, err := newConfiguredADOProvider(c.layout.Root, repository)
			if err != nil {
				return providers.WorkItem{}, fmt.Errorf("build ADO escalation provider for %s/%s: %w", repository.Owner, repository.Name, err)
			}
			return provider.GetWorkItem(ctx, backlog, itemID)
		}
		repository = backlog
	}
	ref := repository.Owner + "/" + repository.Name
	token, err := c.resolver.Resolve(ctx, ref)
	if err != nil {
		return providers.WorkItem{}, fmt.Errorf("resolve escalation-comment token for %s: %w", ref, err)
	}
	c.reg.Register([]byte(token))
	if repository.Provider == providers.ProviderGitea {
		provider, err := newGiteaProviderForStage(c.layout.Root, repository, token)
		if err != nil {
			return providers.WorkItem{}, fmt.Errorf("build gitea escalation provider for %s: %w", ref, err)
		}
		return provider.GetWorkItem(ctx, repository, itemID)
	}
	reader, ok := newEscalationPoster(token).(gate.WorkItemReader)
	if !ok {
		return providers.WorkItem{}, fmt.Errorf("escalation provider for %s cannot read work items", ref)
	}
	return reader.GetWorkItem(ctx, repository, itemID)
}

func (c *escalationCommenter) UpdateComment(ctx context.Context, repository providers.RepositoryRef, commentID, body string) error {
	if repository.Provider == providers.ProviderADO {
		backlog := backlogRepoRefForGaggle(c.layout, repository)
		if !providerconfig.BacklogOnOtherProvider(repository, backlog) {
			return fmt.Errorf("ado work-item comment editing not implemented; streak comment will be posted fresh")
		}
		repository = backlog
	}
	ref := repository.Owner + "/" + repository.Name
	token, err := c.resolver.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("resolve escalation-comment token for %s: %w", ref, err)
	}
	c.reg.Register([]byte(token))
	if repository.Provider == providers.ProviderGitea {
		provider, err := newGiteaProviderForStage(c.layout.Root, repository, token)
		if err != nil {
			return fmt.Errorf("build gitea escalation provider for %s: %w", ref, err)
		}
		if err := c.configureAttribution(ctx, provider); err != nil {
			return err
		}
		return provider.UpdateComment(ctx, repository, commentID, body)
	}
	provider := newEscalationPoster(token)
	if err := c.configureAttribution(ctx, provider); err != nil {
		return err
	}
	return provider.UpdateComment(ctx, repository, commentID, body)
}

// buildEscalationNotifier wires the gate.EscalationNotifier (#20) at the
// composition root — a complete, tested implementation that was never
// constructed, so runner.Config.Escalation stayed nil and a repass-budget
// escalation posted nothing to the driving issue (#312, the same "real seam,
// zero production callers" shape as epic #130). Returns nil when no repo is
// configured. The run supplies its repository to each notification so a
// multi-repo instance resolves and posts through the matching connection.
// Comment-only by deliberate design: the Commenter/UpdateWorkItem seam was
// chosen specifically so escalation never touches the item's status label
// (#63); #20's escalation surfacing is a provider comment on the driving issue,
// not a label change (the goobers:needs-human marker is the curator's output,
// a distinct flow).
func buildEscalationNotifier(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar) *gate.EscalationNotifier {
	if len(cfg.Repos) == 0 {
		return nil
	}
	return &gate.EscalationNotifier{
		Poster: &escalationCommenter{
			resolver:           resolver,
			reg:                reg,
			layout:             l,
			needsHumanAssignee: cfg.NeedsHumanAssignee,
		},
	}
}

// newEscalationPolicy builds the escalation notification policy the runner's
// terminal handlers apply (#3054), posting through an escalationCommenter and
// reading and updating the instance's state through escalationNotifyState.
func newEscalationPolicy(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar) *escalationnotify.Policy {
	return &escalationnotify.Policy{
		Poster: &escalationCommenter{
			resolver:           resolver,
			reg:                reg,
			layout:             l,
			needsHumanAssignee: cfg.NeedsHumanAssignee,
		},
		RunsDir: l.RunsDir(),
		State:   escalationNotifyState{layout: l, cfg: cfg},
	}
}

// buildBlockedHandler wires runner.Config.Blocked (#544/#545/#552) to
// escalationnotify.Policy.Blocked. Returns nil when no repo is configured,
// mirroring buildEscalationNotifier.
func buildBlockedHandler(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar) runner.BlockedHandler {
	if len(cfg.Repos) == 0 {
		return nil
	}
	return newEscalationPolicy(l, cfg, resolver, reg).Blocked
}

// buildFailedHandler wires runner.Config.Failed (#1054) to
// escalationnotify.Policy.Failed. Returns nil when no repo is configured,
// mirroring buildBlockedHandler.
func buildFailedHandler(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar) runner.FailedHandler {
	if len(cfg.Repos) == 0 {
		return nil
	}
	return newEscalationPolicy(l, cfg, resolver, reg).Failed
}

// buildTerminalCircuitBreaker wraps an existing TerminalNotifier with the
// escalated/aborted circuit breaker and the completed-terminal streak resets
// (escalationnotify.Policy.TerminalNotifier). Returns inner unchanged when no
// repo is configured.
func buildTerminalCircuitBreaker(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar, inner runner.TerminalNotifier) runner.TerminalNotifier {
	if len(cfg.Repos) == 0 {
		return inner
	}
	return newEscalationPolicy(l, cfg, resolver, reg).TerminalNotifier(inner)
}

// buildExistingFixHandler wires runner.Config.ExistingFix (#3236) to
// escalationnotify.Policy.ExistingFix. Returns nil when no repo is configured.
func buildExistingFixHandler(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar) runner.ExistingFixHandler {
	if len(cfg.Repos) == 0 {
		return nil
	}
	return newEscalationPolicy(l, cfg, resolver, reg).ExistingFix
}

// escalationNotifyState is escalationnotify.State over the instance layout:
// the claim ledger, blocked records, failure-streak store, circuit-breaker
// outbox, no-work streak and the portal run link.
type escalationNotifyState struct {
	layout instance.Layout
	cfg    *instance.Config
}

func (s escalationNotifyState) ClaimedItemIDs(runID string) ([]string, error) {
	return claimedItemIDsForRun(s.layout, runID)
}

func (s escalationNotifyState) ClaimedItems(runID string) ([]escalationnotify.Item, error) {
	claimed, err := claimedItemsForRun(s.layout, runID)
	if err != nil {
		return nil, err
	}
	items := make([]escalationnotify.Item, 0, len(claimed))
	for _, item := range claimed {
		items = append(items, escalationnotify.Item{ItemID: item.ItemID, Repo: item.Repo})
	}
	return items, nil
}

func (s escalationNotifyState) BacklogRepository(repo providers.RepositoryRef) providers.RepositoryRef {
	return backlogRepoRefForGaggle(s.layout, repo)
}

func (s escalationNotifyState) RecordBlock(b escalationnotify.Block) (blockedCycleResult, error) {
	var cycle blockedCycleResult
	err := updateBlockedRecords(s.layout, func(recs map[string]blockedRecord) bool {
		recordKey := blockedRecordKey(b.Repository, b.ItemID)
		recs[recordKey] = blockedRecord{
			Repository: b.Repository,
			ItemID:     b.ItemID,
			Blockers:   b.Blockers,
			RunID:      b.RunID,
			Stage:      b.Stage,
			Reason:     b.Reason,
			RecordedAt: time.Now().UTC(),
		}
		cycle = findBlockedCycle(recs, recordKey)
		return true
	})
	return cycle, err
}

func (s escalationNotifyState) LoadFailureStreak(ctx context.Context, poster gate.Commenter, repo providers.RepositoryRef, itemID string) (int, error) {
	return loadFailureStreakCount(ctx, poster, s.layout, repo, itemID)
}

func (s escalationNotifyState) WriteFailureStreak(repo providers.RepositoryRef, itemID string, count int, runID, stage string) error {
	return writeFailureStreakCount(s.layout, repo, itemID, count, runID, stage)
}

func (s escalationNotifyState) ReconcileParkOutbox(ctx context.Context, poster gate.Commenter) error {
	return reconcileCircuitBreakerOutbox(ctx, poster, s.layout)
}

func (s escalationNotifyState) RecordParkFailure(repo providers.RepositoryRef, itemID, runID, stage string, streak int, cause error) error {
	return recordCircuitBreakerMutationFailure(s.layout, repo, itemID, runID, stage, streak, cause)
}

func (s escalationNotifyState) ClearParks(repo providers.RepositoryRef, itemID string) error {
	return clearCircuitBreakerMutations(s.layout, repo, itemID)
}

func (s escalationNotifyState) VoidRemediationCharge(ctx context.Context, poster gate.Commenter, runID string) error {
	return voidRemediationChargeForRun(ctx, poster, s.layout, runID)
}

func (s escalationNotifyState) SettleNoWorkStreak(ctx context.Context, poster gate.Commenter, runID, finalState, runURL string) error {
	return settleNoWorkStreak(ctx, poster, s.layout, runID, finalState, runURL)
}

func (s escalationNotifyState) RunURL(runID string) (string, error) {
	return failureRunURL(s.layout, s.cfg, runID)
}

// failureStreakAnnotation marks a runner.annotation event as failure-streak
// bookkeeping (#4364): the count is cached in the instance journal — already
// the durable source of truth for every other cross-run runner decision —
// instead of being reconstructed by listing an item's provider comments on
// every failure. A rate-limited GitHub call while trying to COUNT a failure
// used to be folded into the count it was computing, corrupting the circuit
// breaker with quota exhaustion rather than genuine work failures.
const failureStreakAnnotation = "failure-streak"

// failureStreakKey identifies one item's failure-streak state across
// providers and repos, so two identically-numbered issues in different repos
// never collide.
func failureStreakKey(repo providers.RepositoryRef, itemID string) string {
	return string(repo.Provider) + "/" + repo.Owner + "/" + repo.Name + "#" + itemID
}

// loadFailureStreakCount returns the last persisted failure-streak count for
// an item, or 0 if none has ever been recorded. Reads the Goobers#3025
// scheduler-state key; an absent key migrates on read from the legacy
// failure-streak comment marker (see loadFailureStreakState).
func loadFailureStreakCount(ctx context.Context, poster gate.Commenter, l instance.Layout, repo providers.RepositoryRef, itemID string) (int, error) {
	store, err := openStageStateStore(l)
	if err != nil {
		return 0, fmt.Errorf("open failure-streak state: %w", err)
	}
	record, err := loadFailureStreakState(ctx, store, poster, l, repo, itemID)
	if err != nil {
		return 0, err
	}
	return record.Count, nil
}

// writeFailureStreakCount persists an item's current failure-streak count to
// the Goobers#3025 scheduler-state key — the authoritative value every
// subsequent read (including a status/read-model consumer with no provider
// API access) returns, regardless of what the human-visible comment says.
func writeFailureStreakCount(l instance.Layout, repo providers.RepositoryRef, itemID string, count int, runID, stage string) error {
	store, err := openStageStateStore(l)
	if err != nil {
		return fmt.Errorf("open failure-streak state: %w", err)
	}
	return writeFailureStreakState(stateContext(), store, l, repo, itemID, count, runID, stage)
}

func failureRunURL(l instance.Layout, cfg *instance.Config, runID string) (string, error) {
	address, err := dashboardDaemonAPIAddress(l, cfg.APIListenAddress())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s://%s/#/run/%s", daemonAPIScheme(cfg), address, url.PathEscape(runID)), nil
}

// buildRateLimitedHandler wires runner.Config.RateLimited (#712): records the
// exhausted provider quota into the shared ProviderQuotaState the same
// composition root also hands to the scheduler (via
// localscheduler.WithProviderQuota, schedulerSetup.SchedulerOptions) — the
// Runner and the Scheduler are constructed in different order at the
// composition root, so this pointer, not a Scheduler-owned field, is what
// lets the two agree on one state. pq is never nil (buildSchedulerSetup
// always constructs one); the nil check mirrors the defensive style of this
// file's other optional-dependency handlers.
func buildRateLimitedHandler(pq *localscheduler.ProviderQuotaState) runner.RateLimitedHandler {
	if pq == nil {
		return nil
	}
	return func(_ context.Context, o runner.RateLimitedOutcome) error {
		pq.RecordExhausted(o.ResetAt)
		return nil
	}
}

// claimedItemIDsForRun resolves the backlog item(s) a run currently claims —
// the driving-issue fallback for a run started without an Item snapshot. Read
// under the claim lock like every other ledger access; the blocked handler
// runs before FinalizeTerminal, so the claims are still held here.
func claimedItemIDsForRun(l instance.Layout, runID string) ([]string, error) {
	var ids []string
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationRunLookup, func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		for _, entry := range ledger.ForRunAll(runID) {
			ids = append(ids, entry.ItemID)
		}
		return nil
	})
	return ids, err
}
