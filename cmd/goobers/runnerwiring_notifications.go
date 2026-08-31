package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

// newEscalationPoster constructs the provider the escalation notifier posts
// through — a package var so tests substitute a fake without a real GitHub
// client (mirrors newPRPoller).
var newEscalationPoster = func(token string) gate.Commenter { return providers.NewGitHubProvider(token) }

// daemonBacklogTarget is the daemon-side answer to the two questions every
// work-item mutation has to agree on: which container does this address, and
// which credential reaches it. stageBacklogRef answers both for a stage from
// the injected env; this answers both for the daemon process, which has only
// the gaggle-scoped layout.
type daemonBacklogTarget struct {
	// routed is the repository the run targets — retained because ADO's
	// provider is organization-scoped and its config lookup matches on the
	// routed tuple, and because it is the credential fallback.
	routed providers.RepositoryRef
	// repo is the container work items actually live in.
	repo          providers.RepositoryRef
	connectionRef string
}

// resolveDaemonBacklogTarget derives the addressing repository AND the
// credential connection from ONE gaggleBacklogRef call, which is what makes
// "built for the backlog" and "authenticated as the backlog" inseparable in the
// daemon exactly as backlogStageProviderOptions makes them inseparable for a
// stage. Resolving them independently is how a handler ends up reaching the
// right repository with the wrong token — invisible in the same-repository
// majority (one token serves both) and an opaque 404/403 in precisely the
// cross-repository topology this exists to support.
//
// Both fall back to the routed repository when the gaggle declares no distinct
// backlog, so a same-repository gaggle keeps exactly its previous behavior.
func resolveDaemonBacklogTarget(l instance.Layout, routed providers.RepositoryRef) daemonBacklogTarget {
	ref, _ := gaggleBacklogRef(l, routed)
	target := daemonBacklogTarget{routed: routed, repo: routed, connectionRef: ref.ConnectionRef}
	if repo, err := backlogRepositoryRef(ref, routed); err == nil {
		target.repo = repo
	}
	return target
}

// credentialRefs is the ordered list of resolver refs that may hold the
// credential this target's provider must present, most specific first:
//
//  1. the declared connection (buildCredentials registers it under
//     credentialRefName(ConnectionCredentialKey(name)));
//  2. the backlog repository's own binding, for a cross-repository backlog
//     that is itself a configured repo but names no connection;
//  3. the routed repository's binding — the pre-routing behavior, and the
//     correct answer whenever backlog and project coincide.
//
// Falling through rather than failing on a missing connection credential
// mirrors stageProviderToken's connectionToken fallback: a gaggle may name a
// connection purely for documentation/validation while project and backlog
// live under one account and one token.
func (t daemonBacklogTarget) credentialRefs() []string {
	refs := make([]string, 0, 3)
	if t.connectionRef != "" {
		refs = append(refs, credentialRefName(credentials.ConnectionCredentialKey(t.connectionRef)))
	}
	for _, repo := range []providers.RepositoryRef{t.repo, t.routed} {
		if repo.Owner == "" || repo.Name == "" {
			continue
		}
		ref := repo.Owner + "/" + repo.Name
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// escalationCommenter is the gate.Commenter the runner posts escalation
// comments through (#312). Like buildCIPollExecutor it resolves the org-repo
// token per call — honoring credentials.Resolver's re-read-on-resolve rotation
// contract rather than capturing a token once at daemon startup — registers it
// for scrubbing, then posts through a freshly-authenticated provider.
//
// Every method resolves the backlog target first, so the repository addressed
// and the credential presented always come from the same spec.backlog. On
// Azure DevOps there is no static repo token to resolve (azure-cli auth shells
// out to `az`), so the ADO branch builds a provider straight from instance
// config (adoauth). Without this every ADO run's failure/park/escalation
// handler no-ops (token ref not found), leaking the goobers/status:claimed
// marker and never applying needs-human.
type escalationCommenter struct {
	resolver           credentials.Resolver
	reg                runner.SecretRegistrar
	layout             instance.Layout
	needsHumanAssignee string
}

// backlogToken resolves the credential this daemon handler must present to the
// backlog, re-resolved on every call (rotation) and registered for scrubbing
// before it can reach a journal. Refs are tried in credentialRefs order and a
// ref the instance simply does not configure falls through to the next.
func (c *escalationCommenter) backlogToken(ctx context.Context, target daemonBacklogTarget) (string, error) {
	refs := target.credentialRefs()
	var lastErr error
	for _, ref := range refs {
		token, err := c.resolver.Resolve(ctx, ref)
		if err != nil {
			lastErr = err
			if errors.Is(err, credentials.ErrTokenRefNotFound) {
				continue
			}
			return "", fmt.Errorf("resolve escalation-comment token for %s: %w", ref, err)
		}
		c.reg.Register([]byte(token))
		return token, nil
	}
	if lastErr == nil {
		lastErr = credentials.ErrTokenRefNotFound
	}
	return "", fmt.Errorf("resolve escalation-comment token for %s: %w", strings.Join(refs, ", "), lastErr)
}

// adoProvider is the ADO half of the connection-aware daemon construction. It
// redirects ONLY PAT auth, exactly like newADOProviderForConnection does for a
// stage: the Entra-backed kinds authenticate as an ambient identity with no
// token ref to substitute. With no connection credential configured it goes
// through the unchanged newADOProviderForStage seam, so a gaggle that declares
// no backlog connection — and every test that stubs that seam — is untouched.
func (c *escalationCommenter) adoProvider(target daemonBacklogTarget) (*providers.ADOProvider, error) {
	if repo, ok := adoBacklogConnectionRepo(c.layout.Root, target); ok {
		return adoauth.Provider(repo, nil, nil, nil, nil, nil)
	}
	return newADOProviderForStage(c.layout.Root, target.routed)
}

// adoBacklogConnectionRepo reports the ADO RepoRef to build the backlog
// provider from when — and only when — the gaggle declares a backlog
// connection, the instance configures a credential for it, and the repo uses
// PAT auth. The connection's own TokenRef is substituted rather than a resolved
// string so adoauth keeps re-reading it per call (rotation), matching how the
// repo's own token behaves.
func adoBacklogConnectionRepo(root string, target daemonBacklogTarget) (instance.RepoRef, bool) {
	if target.connectionRef == "" || target.repo.Provider != providers.ProviderADO {
		return instance.RepoRef{}, false
	}
	repo, err := adoRepoRefForStage(root, target.repo)
	if err != nil {
		return instance.RepoRef{}, false
	}
	kind := instance.ADOAuthPAT
	if repo.Auth != nil {
		kind = repo.Auth.Kind
	}
	if kind != instance.ADOAuthPAT {
		return instance.RepoRef{}, false
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		return instance.RepoRef{}, false
	}
	for _, grant := range cfg.Credentials {
		if grant.Connection != target.connectionRef || !grant.Token.Configured() {
			continue
		}
		repo.Token = grant.Token
		return repo, true
	}
	return instance.RepoRef{}, false
}

func (c *escalationCommenter) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	// PR remediation uses pr/<number> as its internal claim key; provider work
	// item endpoints use the shared bare issue/PR number.
	req.ID = blockedLookupID(req.ID)
	req = withNeedsHumanAssignee(req, c.needsHumanAssignee)
	target := resolveDaemonBacklogTarget(c.layout, req.Repository)
	// The mutation is addressed at the backlog for EVERY provider, not just
	// ADO: a GitHub or Gitea gaggle whose backlog lives in another repository
	// was previously parked/labelled/commented on the code repo instead.
	req.Repository = target.repo
	switch target.repo.Provider {
	case providers.ProviderADO:
		provider, err := c.adoProvider(target)
		if err != nil {
			return providers.WorkItem{}, fmt.Errorf("build ADO escalation provider for %s/%s: %w", target.routed.Owner, target.routed.Name, err)
		}
		return provider.UpdateWorkItem(ctx, req)
	case providers.ProviderGitea:
		// Gitea authenticates with a static token like GitHub (resolved per call
		// through the rotation-aware resolver), but the mutation must reach the
		// self-hosted forge — newGiteaProviderForStage resolves its BaseURL from
		// instance config. The claim marker is the plain LabelClaimed (as GitHub),
		// so no ADO status-label rewrite is needed.
		token, err := c.backlogToken(ctx, target)
		if err != nil {
			return providers.WorkItem{}, err
		}
		provider, err := newGiteaProviderForStage(c.layout.Root, target.repo, token)
		if err != nil {
			return providers.WorkItem{}, fmt.Errorf("build gitea escalation provider for %s/%s: %w", target.repo.Owner, target.repo.Name, err)
		}
		return provider.UpdateWorkItem(ctx, req)
	}
	token, err := c.backlogToken(ctx, target)
	if err != nil {
		return providers.WorkItem{}, err
	}
	return newEscalationPoster(token).UpdateWorkItem(ctx, req)
}

func (c *escalationCommenter) ListComments(ctx context.Context, repository providers.RepositoryRef, itemID string) ([]providers.Comment, error) {
	itemID = blockedLookupID(itemID)
	target := resolveDaemonBacklogTarget(c.layout, repository)
	switch target.repo.Provider {
	case providers.ProviderADO:
		provider, err := c.adoProvider(target)
		if err != nil {
			return nil, fmt.Errorf("build ADO escalation provider for %s/%s: %w", target.routed.Owner, target.routed.Name, err)
		}
		return provider.ListComments(ctx, target.repo, itemID)
	case providers.ProviderGitea:
		token, err := c.backlogToken(ctx, target)
		if err != nil {
			return nil, err
		}
		provider, err := newGiteaProviderForStage(c.layout.Root, target.repo, token)
		if err != nil {
			return nil, fmt.Errorf("build gitea escalation provider for %s/%s: %w", target.repo.Owner, target.repo.Name, err)
		}
		return provider.ListComments(ctx, target.repo, itemID)
	}
	token, err := c.backlogToken(ctx, target)
	if err != nil {
		return nil, err
	}
	return newEscalationPoster(token).ListComments(ctx, target.repo, itemID)
}

func (c *escalationCommenter) UpdateComment(ctx context.Context, repository providers.RepositoryRef, commentID, body string) error {
	target := resolveDaemonBacklogTarget(c.layout, repository)
	switch target.repo.Provider {
	case providers.ProviderADO:
		return fmt.Errorf("ado work-item comment editing not implemented; streak comment will be posted fresh")
	case providers.ProviderGitea:
		token, err := c.backlogToken(ctx, target)
		if err != nil {
			return err
		}
		provider, err := newGiteaProviderForStage(c.layout.Root, target.repo, token)
		if err != nil {
			return fmt.Errorf("build gitea escalation provider for %s/%s: %w", target.repo.Owner, target.repo.Name, err)
		}
		return provider.UpdateComment(ctx, target.repo, commentID, body)
	}
	token, err := c.backlogToken(ctx, target)
	if err != nil {
		return err
	}
	return newEscalationPoster(token).UpdateComment(ctx, target.repo, commentID, body)
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

// buildBlockedHandler wires runner.Config.Blocked (#544/#545/#552): the
// instance-level consequences of a stage reporting status "blocked". Returns
// nil when no repo is configured, mirroring buildEscalationNotifier.
// Every blocked driving issue is parked (swap off goobers:ready and the
// provider-visible claim marker) per the #544 ruling / #539 convention. This
// prevents the released claim from making the same item immediately eligible
// again.
//
// The park label depends on whether the stage named a blocker (#2028): a
// named, non-cyclic blocker is goobers:blocked-on-sibling — a self-healing
// dependency park, not a decision only a human can make; the record below is
// what actually self-heals it (filterBlockedEligibility, blockedrecords.go),
// the label just needs to say so. An unattributed block (no blocker named) or
// a detected circular dependency is goobers:needs-human — the runner can't
// resolve either on its own, so it genuinely is a human decision.
//
// When the stage also references blockers through outputs.blockedBy, record
// them in scheduler/blocked.json so #552's selection guard still protects the
// issue if a human re-promotes it before every dependency closes. Blockers
// naming the driving item itself are dropped first (#2961) — an item cannot
// depend on itself, and persisting that self-edge makes findBlockedCycle
// report a one-node cycle and park the issue needs-human over a dependency
// that does not exist. If a new record closes a real cycle, every issue in
// that cycle is parked goobers:needs-human and receives a cycle-specific
// comment for human resolution. The runner's shared EscalationNotifier owns
// the normal explanatory provider comment.
//
// The handler runs before FinalizeTerminal releases the run's claims, so a
// run with no StartInput.Item (scheduled/fan-out implementation runs claim
// their item mid-run) resolves its driving item(s) from the claim ledger by
// run id. Best-effort per item: one item's provider failure doesn't skip the
// rest; the joined error is journaled by the runner (blocked_handling_failed),
// never fatal to the terminal transition.
func buildBlockedHandler(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar) runner.BlockedHandler {
	if len(cfg.Repos) == 0 {
		return nil
	}
	poster := &escalationCommenter{
		resolver:           resolver,
		reg:                reg,
		layout:             l,
		needsHumanAssignee: cfg.NeedsHumanAssignee,
	}

	return func(ctx context.Context, o runner.BlockedOutcome) error {
		itemIDs := []string{o.ItemID}
		if o.ItemID == "" {
			ids, err := claimedItemIDsForRun(l, o.RunID)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				// No driving item anywhere (a producer run) — nothing to
				// record or park; the journaled blocked_by_agent cause and the
				// escalated phase are the whole story.
				return nil
			}
			itemIDs = ids
		}

		var errs []error
		repoRef := providers.RepositoryRef{
			Provider: providers.ProviderKind(o.RepoRef.Provider),
			Owner:    o.RepoRef.Owner,
			Name:     o.RepoRef.Name,
		}
		if blockedRepositoryEmpty(repoRef) {
			return fmt.Errorf("blocked outcome for run %s has no repository", o.RunID)
		}
		// Scope blocked records to the backlog project, not the code repo.
		// Work items live in the gaggle's backlog project (e.g. "Example Backlog"), which
		// is a different ADO project than the code repo ("Example Service").
		// The selection guard (filterBlockedEligibility) evaluates records
		// against the backlog repo, so records must be keyed/stored under the
		// backlog repo or a parked parent is never skipped and gets re-claimed.
		// Idempotent for GitHub (backlog == code repo) and re-applied safely by
		// escalationCommenter before the work-item call.
		repoRef = backlogRepoRefForGaggle(l, repoRef)
		for _, itemID := range itemIDs {
			// #2961: an item can never be its own blocker. The runner already
			// drops the self-reference when the run carried its driving item,
			// but a run that claims its item(s) mid-run resolves them here, so
			// the same guard has to apply per item — otherwise a self-edge
			// reaches blocked.json and findBlockedCycle parks the issue
			// needs-human for a dependency cycle that does not exist.
			blockers, _ := runner.FilterSelfBlockers(o.Blockers, itemID)
			// #2028: a named blocker is a self-healing dependency park
			// (blocked-on-sibling), not a human decision; only an
			// unattributed block stays needs-human. A detected cycle
			// overrides this below with its own needs-human cycleReq.
			// A block whose only named blocker was the item itself is
			// unattributed once filtered, so it correctly stays needs-human.
			label := providers.LabelNeedsHuman
			if len(blockers) > 0 {
				label = blockedOnSiblingLabel
			}
			req := providers.UpdateWorkItemRequest{
				Repository:   repoRef,
				ID:           itemID,
				AddLabels:    []string{label},
				RemoveLabels: []string{providers.LabelReady, providers.LabelClaimed},
			}
			if len(blockers) > 0 {
				var cycle blockedCycleResult
				if err := updateBlockedRecords(l, func(recs map[string]blockedRecord) bool {
					recordKey := blockedRecordKey(repoRef, itemID)
					recs[recordKey] = blockedRecord{
						Repository: repoRef,
						ItemID:     itemID,
						Blockers:   blockers,
						RunID:      o.RunID,
						Stage:      o.Stage,
						Reason:     o.Reason,
						RecordedAt: time.Now().UTC(),
					}
					cycle = findBlockedCycle(recs, recordKey)
					return true
				}); err != nil {
					errs = append(errs, fmt.Errorf("record block for %s: %w", itemID, err))
				}
				if len(cycle.Affected) > 0 {
					comments := blockedCycleComments(cycle)
					for _, cycleItem := range cycle.Affected {
						for _, comment := range comments {
							cycleReq := providers.UpdateWorkItemRequest{
								Repository:   cycleItem.Repository,
								ID:           cycleItem.ItemID,
								Comment:      comment,
								AddLabels:    []string{providers.LabelNeedsHuman},
								RemoveLabels: []string{providers.LabelReady, providers.LabelClaimed},
							}
							if _, err := poster.UpdateWorkItem(ctx, cycleReq); err != nil {
								errs = append(errs, fmt.Errorf("escalate circular dependency on %s#%s: %w", cycleItem.Repository.Name, cycleItem.ItemID, err))
							}
						}
					}
					continue
				}
			}
			if _, err := poster.UpdateWorkItem(ctx, req); err != nil {
				errs = append(errs, fmt.Errorf("park blocked item %s#%s: %w", repoRef.Name, itemID, err))
			}
		}
		return errors.Join(errs...)
	}
}

// buildFailedHandler wires runner.Config.Failed (#1054): the instance-level
// consequence of a run reaching terminal PhaseFailed. Returns nil when no repo
// is configured, mirroring buildBlockedHandler. Leaves a human-visible trace on
// the driving item — a comment recording a stable failure code and the run id —
// so repeated terminal failures on the same item accumulate a countable signal
// instead of the item silently returning to goobers:ready with no record.
// Detailed causes remain in the local run trace because execution errors can
// contain harness argv, prompts, credentials, environment values, or context.
//
// Circuit breaker: after failureStreakThreshold consecutive terminal failures
// on the same item, applies goobers:needs-human and removes goobers:ready so
// the retry loop stops. The threshold is counted via a single editable
// failure-streak comment on the issue (one comment, updated in place, instead
// of one per run).
//
// Like buildBlockedHandler, the handler runs before FinalizeTerminal releases
// the run's claims, so it resolves the driving item(s) from the claim ledger by
// run id. Best-effort per item: one item's provider failure doesn't skip the
// rest; the joined error is journaled by the runner (failed_handling_failed),
// never fatal to the terminal transition.
func buildFailedHandler(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar) runner.FailedHandler {
	if len(cfg.Repos) == 0 {
		return nil
	}
	poster := &escalationCommenter{
		resolver:           resolver,
		reg:                reg,
		layout:             l,
		needsHumanAssignee: cfg.NeedsHumanAssignee,
	}

	return func(ctx context.Context, o runner.FailedOutcome) error {
		repoRef := providers.RepositoryRef{
			Provider: providers.ProviderKind(o.RepoRef.Provider),
			Owner:    o.RepoRef.Owner,
			Name:     o.RepoRef.Name,
		}
		runURL, _ := failureRunURL(l, cfg, o.RunID)
		return applyCircuitBreaker(ctx, poster, l, repoRef, o.RunID, o.Stage, runURL)
	}
}

const failureStreakThreshold = 3

// applyCircuitBreaker increments the failure streak for each claimed item and
// parks the issue (needs-human + remove ready) once the threshold is reached.
// Shared by buildFailedHandler (PhaseFailed) and buildTerminalCircuitBreaker
// (PhaseEscalated/PhaseAborted) so that ALL non-completed terminals count
// toward the same streak.
//
// The streak is counted, commented, and enforced on the BACKLOG item — for
// every provider, not just ADO. The rewrite belongs here as well as inside
// escalationCommenter because this function's own bookkeeping is repo-scoped:
// gate.CountFailureStreak reads the comment thread of whatever repository it is
// handed, so counting against the code repo while the comment lands on the
// backlog would restart the streak at 1 on every terminal failure and the
// breaker would never trip. backlogRepoRefForGaggle is idempotent, so a
// caller that already resolved the backlog loses nothing.
func applyCircuitBreaker(ctx context.Context, poster gate.Commenter, l instance.Layout, repoRef providers.RepositoryRef, runID, stage, runURL string) error {
	itemIDs, err := claimedItemIDsForRun(l, runID)
	if err != nil {
		return err
	}
	if len(itemIDs) == 0 {
		return nil
	}
	repoRef = backlogRepoRefForGaggle(l, repoRef)
	var errs []error
	for _, itemID := range itemIDs {
		prevCount, _, countErr := gate.CountFailureStreak(ctx, poster, repoRef, itemID)
		if countErr != nil {
			errs = append(errs, fmt.Errorf("count failure streak on %s#%s: %w", repoRef.Name, itemID, countErr))
			prevCount = 0
		}
		count := prevCount + 1

		if err := gate.UpsertFailureComment(ctx, poster, repoRef, itemID, count, stage, runID, runURL); err != nil {
			errs = append(errs, fmt.Errorf("upsert failure comment on %s#%s: %w", repoRef.Name, itemID, err))
		}

		if count >= failureStreakThreshold {
			if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
				Repository:   repoRef,
				ID:           itemID,
				AddLabels:    []string{providers.LabelNeedsHuman},
				RemoveLabels: []string{providers.LabelReady},
			}); err != nil {
				errs = append(errs, fmt.Errorf("apply circuit breaker on %s#%s: %w", repoRef.Name, itemID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// buildTerminalCircuitBreaker wraps an existing TerminalNotifier with circuit
// breaker logic for PhaseEscalated and PhaseAborted. PhaseFailed is handled by
// buildFailedHandler (which calls applyCircuitBreaker directly), so this
// wrapper skips PhaseFailed to avoid double-counting. Returns nil when no repo
// is configured.
func buildTerminalCircuitBreaker(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar, inner runner.TerminalNotifier) runner.TerminalNotifier {
	if len(cfg.Repos) == 0 {
		return inner
	}
	poster := &escalationCommenter{
		resolver:           resolver,
		reg:                reg,
		layout:             l,
		needsHumanAssignee: cfg.NeedsHumanAssignee,
	}
	repo := cfg.Repos[0]
	repoRef := providers.RepositoryRef{
		Provider: providers.ProviderKind(repo.Provider),
		Owner:    repo.Owner,
		Name:     repo.Name,
	}

	return func(runID string, phase journal.RunPhase, finalState string) error {
		if phase == journal.PhaseEscalated || phase == journal.PhaseAborted {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			runURL, _ := failureRunURL(l, cfg, runID)
			_ = applyCircuitBreaker(ctx, poster, l, repoRef, runID, finalState, runURL)
		}
		if inner != nil {
			return inner(runID, phase, finalState)
		}
		return nil
	}
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

// issueRefList renders issue numbers as "#441, #442" for provider comments.
func issueRefList(numbers []string) string {
	out := make([]byte, 0, len(numbers)*6)
	for i, n := range numbers {
		if i > 0 {
			out = append(out, ", "...)
		}
		out = append(out, '#')
		out = append(out, n...)
	}
	return string(out)
}

const cyclePathSeparator = " -> "

func issueCyclePath(numbers []string) string {
	var out strings.Builder
	for i, n := range numbers {
		if i > 0 {
			out.WriteString(cyclePathSeparator)
		}
		out.WriteByte('#')
		out.WriteString(n)
	}
	return out.String()
}

func issueCyclePathLength(numbers []string, maxLength int) (int, bool) {
	length := 0
	for i, number := range numbers {
		addition := 1 + len(number)
		if i > 0 {
			addition += len(cyclePathSeparator)
		}
		if addition > maxLength-length {
			return 0, false
		}
		length += addition
	}
	return length, true
}

func boundedIssueCyclePath(numbers []string, maxLength int) (string, bool) {
	if _, fits := issueCyclePathLength(numbers, maxLength); fits {
		return issueCyclePath(numbers), false
	}
	return truncatedIssueCyclePath(numbers, maxLength), true
}

func truncatedIssueCyclePath(numbers []string, maxLength int) string {
	if len(numbers) == 0 || maxLength <= 0 {
		return ""
	}

	bestHead, bestIdentified := 0, -1
	bestTail := false
	prefixLength := 0
	for head := 0; head < len(numbers); head++ {
		consider := func(includeTail bool) {
			omitted := len(numbers) - head
			identified := head
			if includeTail {
				omitted--
				identified++
			}
			if omitted <= 0 {
				return
			}

			length := prefixLength
			if head > 0 {
				length += len(cyclePathSeparator)
			}
			length += len(cycleMembersOmitted(omitted))
			if includeTail {
				length += len(cyclePathSeparator) + 1 + len(numbers[len(numbers)-1])
			}
			if length <= maxLength &&
				(identified > bestIdentified || identified == bestIdentified && head > bestHead) {
				bestHead = head
				bestTail = includeTail
				bestIdentified = identified
			}
		}

		consider(false)
		consider(head < len(numbers)-1)

		addition := 1 + len(numbers[head])
		if head > 0 {
			addition += len(cyclePathSeparator)
		}
		prefixLength += addition
		if prefixLength > maxLength {
			break
		}
	}
	if bestIdentified < 0 {
		return ""
	}

	omitted := len(numbers) - bestHead
	if bestTail {
		omitted--
	}
	parts := make([]string, 0, bestHead+2)
	for _, number := range numbers[:bestHead] {
		parts = append(parts, "#"+number)
	}
	parts = append(parts, cycleMembersOmitted(omitted))
	if bestTail {
		parts = append(parts, "#"+numbers[len(numbers)-1])
	}
	return strings.Join(parts, cyclePathSeparator)
}

func cycleMembersOmitted(count int) string {
	return fmt.Sprintf("[%d cycle members omitted]", count)
}

const maxBlockedCycleCommentLength = 2000

func blockedCycleComment(paths [][]string, morePaths bool) string {
	const prefix = "Goobers detected circular issue dependencies. Representative cycles: "
	const additionalPathsOmitted = "additional cycle paths omitted"
	suffix := fmt.Sprintf(
		". Every issue in the cycle has been marked `%s` and removed from `%s` for human resolution.",
		providers.LabelNeedsHuman, providers.LabelReady,
	)
	available := maxBlockedCycleCommentLength - len(prefix) - len(suffix)
	if summaries, ok := completeCycleSummaries(paths, morePaths, available, additionalPathsOmitted); ok {
		return prefix + summaries + suffix
	}

	var summaries strings.Builder
	included := 0
	for i, path := range paths {
		separatorLength := 0
		if summaries.Len() > 0 {
			separatorLength = 2
		}

		reservedNoticeLength := 0
		if morePaths || i < len(paths)-1 {
			reservedNoticeLength = 2 + len(additionalPathsOmitted)
		}
		pathBudget := available - summaries.Len() - separatorLength - reservedNoticeLength
		summary, truncated := boundedIssueCyclePath(path, pathBudget)
		if summary == "" {
			break
		}
		if separatorLength > 0 {
			summaries.WriteString("; ")
		}
		summaries.WriteString(summary)
		included++
		if truncated {
			break
		}
	}

	if morePaths || included < len(paths) {
		if summaries.Len() > 0 {
			summaries.WriteString("; ")
		}
		summaries.WriteString(additionalPathsOmitted)
	}
	return prefix + summaries.String() + suffix
}

func blockedCycleComments(cycle blockedCycleResult) []string {
	report := blockedCycleComment(cycle.Paths, cycle.MorePaths)
	itemIDs := make([]string, len(cycle.Affected))
	for i, item := range cycle.Affected {
		itemIDs[i] = item.ItemID
	}

	memberList := " Affected issues: " + issueRefList(itemIDs) + "."
	if len(report)+len(memberList) <= maxBlockedCycleCommentLength {
		return []string{report + memberList}
	}

	comments := []string{report}
	const prefix = "Affected issues in this dependency cycle: "
	var current strings.Builder
	current.WriteString(prefix)
	for _, itemID := range itemIDs {
		separator := ""
		if current.Len() > len(prefix) {
			separator = ", "
		}
		reference := "#" + itemID
		if current.Len()+len(separator)+len(reference)+1 > maxBlockedCycleCommentLength {
			current.WriteByte('.')
			comments = append(comments, current.String())
			current.Reset()
			current.WriteString(prefix)
			separator = ""
		}
		current.WriteString(separator)
		current.WriteString(reference)
	}
	if current.Len() > len(prefix) {
		current.WriteByte('.')
		comments = append(comments, current.String())
	}
	return comments
}

func completeCycleSummaries(paths [][]string, morePaths bool, maxLength int, additionalPathsOmitted string) (string, bool) {
	total := 0
	for i, path := range paths {
		separatorLength := 0
		if i > 0 {
			separatorLength = 2
		}
		pathLength, fits := issueCyclePathLength(path, maxLength-total-separatorLength)
		if !fits {
			return "", false
		}
		total += separatorLength + pathLength
	}
	if morePaths {
		separatorLength := 0
		if len(paths) > 0 {
			separatorLength = 2
		}
		if len(additionalPathsOmitted) > maxLength-total-separatorLength {
			return "", false
		}
		total += separatorLength + len(additionalPathsOmitted)
	}

	var summaries strings.Builder
	summaries.Grow(total)
	for i, path := range paths {
		if i > 0 {
			summaries.WriteString("; ")
		}
		summaries.WriteString(issueCyclePath(path))
	}
	if morePaths {
		if summaries.Len() > 0 {
			summaries.WriteString("; ")
		}
		summaries.WriteString(additionalPathsOmitted)
	}
	return summaries.String(), true
}
