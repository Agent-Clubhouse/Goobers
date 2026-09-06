package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
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
		provider, err := newADOProviderForStage(c.layout.Root, req.Repository)
		if err != nil {
			return providers.WorkItem{}, fmt.Errorf("build ADO escalation provider for %s/%s: %w", req.Repository.Owner, req.Repository.Name, err)
		}
		if err := c.configureAttribution(ctx, provider); err != nil {
			return providers.WorkItem{}, err
		}
		req.Repository = backlogRepoRefForGaggle(c.layout, req.Repository)
		return provider.UpdateWorkItem(ctx, req)
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

func (c *escalationCommenter) ListComments(ctx context.Context, repository providers.RepositoryRef, itemID string) ([]providers.Comment, error) {
	itemID = blockedLookupID(itemID)
	if repository.Provider == providers.ProviderADO {
		provider, err := newADOProviderForStage(c.layout.Root, repository)
		if err != nil {
			return nil, fmt.Errorf("build ADO escalation provider for %s/%s: %w", repository.Owner, repository.Name, err)
		}
		return provider.ListComments(ctx, backlogRepoRefForGaggle(c.layout, repository), itemID)
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

func (c *escalationCommenter) UpdateComment(ctx context.Context, repository providers.RepositoryRef, commentID, body string) error {
	if repository.Provider == providers.ProviderADO {
		return fmt.Errorf("ado work-item comment editing not implemented; streak comment will be posted fresh")
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
		ctx = withDaemonAttributionTask(ctx, o.Stage)
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
					comments := blockedcycle.Comments(cycle)
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
		ctx = withDaemonAttributionTask(ctx, o.Stage)
		// #3361/#3364: an infra-fault terminal (credential materialization, git,
		// network, lock contention) is weather, not evidence about the item —
		// it must not accumulate failure-streak strikes that eventually park
		// the item goobers:needs-human. The item returns to the pool untouched
		// and the scheduler's auth circuit / quota gates own the retry cadence.
		// Item-judgment terminals (a verified ISSUE_NOT_APPLICABLE refusal,
		// #3363) are likewise not work failures. Timeout deliberately still
		// counts: a recurring harness session timeout is this circuit
		// breaker's motivating case (#1054).
		if class := telemetry.ClassifyError(o.Code); class.InfraFault() || class == telemetry.ErrorClassItemJudgment {
			return nil
		}
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
// an item, or 0 if none has ever been recorded. It scans the instance journal
// for the newest matching failure-streak annotation — the same append-only,
// scan-for-the-latest-marker pattern the provider-comment version used,
// just against durable local state instead of a live network call.
func loadFailureStreakCount(l instance.Layout, repo providers.RepositoryRef, itemID string) (int, error) {
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		return 0, fmt.Errorf("read instance log for failure streak: %w", err)
	}
	key := failureStreakKey(repo, itemID)
	count := 0
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["annotation"] != failureStreakAnnotation {
			continue
		}
		if event.Runner["key"] != key {
			continue
		}
		// Runner values round-trip through JSON, so a persisted int decodes
		// as float64 here.
		if n, ok := event.Runner["count"].(float64); ok {
			count = int(n)
		}
	}
	return count, nil
}

// writeFailureStreakCount persists an item's current failure-streak count to
// the instance journal.
func writeFailureStreakCount(l instance.Layout, repo providers.RepositoryRef, itemID string, count int, runID, stage string) error {
	annotations, err := openStageAnnotator(l)
	if err != nil {
		return fmt.Errorf("open instance log for failure streak: %w", err)
	}
	defer func() { _ = annotations.Close() }()
	return annotations.Append(journal.Event{
		Type:  journal.EventRunnerAnnotation,
		RunID: runID,
		Stage: stage,
		Runner: map[string]any{
			"annotation": failureStreakAnnotation,
			"key":        failureStreakKey(repo, itemID),
			"count":      count,
		},
	})
}

// applyCircuitBreaker increments the failure streak for each claimed item and
// parks the issue (needs-human + remove ready) once the threshold is reached.
// Shared by buildFailedHandler (PhaseFailed) and buildTerminalCircuitBreaker
// (PhaseEscalated/PhaseAborted) so that ALL non-completed terminals count
// toward the same streak.
//
// A park whose provider mutation fails is persisted to the circuit-breaker
// outbox (#3646) and retried here on the next terminal: discarding it left the
// item goobers:ready with no durable evidence the protection had been
// attempted, so unhealthy work kept churning.
func applyCircuitBreaker(ctx context.Context, poster gate.Commenter, l instance.Layout, repoRef providers.RepositoryRef, runID, stage, runURL string) error {
	var errs []error
	if err := reconcileCircuitBreakerOutbox(ctx, poster, l); err != nil {
		errs = append(errs, err)
	}
	itemIDs, err := claimedItemIDsForRun(l, runID)
	if err != nil {
		errs = append(errs, err)
		return errors.Join(errs...)
	}
	if len(itemIDs) == 0 {
		return errors.Join(errs...)
	}
	for _, itemID := range itemIDs {
		prevCount, loadErr := loadFailureStreakCount(l, repoRef, itemID)
		if loadErr != nil {
			errs = append(errs, fmt.Errorf("load failure streak state on %s#%s: %w", repoRef.Name, itemID, loadErr))
			continue
		}
		count := prevCount + 1

		if err := gate.UpsertFailureComment(ctx, poster, repoRef, itemID, count, stage, runID, runURL); err != nil {
			// A rate-limited (or otherwise failed) comment write must not
			// advance the persisted streak: the human-visible comment and the
			// cached count would drift apart, and "I couldn't post" is not
			// evidence the item failed again (#4364).
			errs = append(errs, fmt.Errorf("upsert failure comment on %s#%s: %w", repoRef.Name, itemID, err))
			continue
		}
		if err := writeFailureStreakCount(l, repoRef, itemID, count, runID, stage); err != nil {
			errs = append(errs, fmt.Errorf("persist failure streak state on %s#%s: %w", repoRef.Name, itemID, err))
		}

		if count >= failureStreakThreshold {
			if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
				Repository:   repoRef,
				ID:           itemID,
				AddLabels:    []string{providers.LabelNeedsHuman},
				RemoveLabels: []string{providers.LabelReady},
			}); err != nil {
				errs = append(errs, fmt.Errorf("apply circuit breaker on %s#%s: %w", repoRef.Name, itemID, err))
				if rerr := recordCircuitBreakerMutationFailure(l, repoRef, itemID, runID, stage, count, err); rerr != nil {
					errs = append(errs, fmt.Errorf("persist circuit breaker park for %s#%s: %w", repoRef.Name, itemID, rerr))
				}
			} else if cerr := clearCircuitBreakerMutations(l, repoRef, itemID); cerr != nil {
				errs = append(errs, fmt.Errorf("clear circuit breaker park for %s#%s: %w", repoRef.Name, itemID, cerr))
			}
		}
	}
	return errors.Join(errs...)
}

func resetCircuitBreaker(ctx context.Context, poster gate.Commenter, l instance.Layout, repoRef providers.RepositoryRef, runID, runURL string) error {
	itemIDs, err := claimedItemIDsForRun(l, runID)
	if err != nil {
		return err
	}
	var errs []error
	for _, itemID := range itemIDs {
		if err := gate.ResetFailureComment(ctx, poster, repoRef, itemID, runID, runURL); err != nil {
			errs = append(errs, fmt.Errorf("reset failure streak on %s#%s: %w", repoRef.Name, itemID, err))
		}
		if err := writeFailureStreakCount(l, repoRef, itemID, 0, runID, ""); err != nil {
			errs = append(errs, fmt.Errorf("reset failure streak state on %s#%s: %w", repoRef.Name, itemID, err))
		}
	}
	// A completed run resets the streak that motivated any still-pending park
	// for these items, so the outbox entry is moot rather than owed (#3646).
	if err := clearCircuitBreakerMutations(l, repoRef, itemIDs...); err != nil {
		errs = append(errs, fmt.Errorf("clear circuit breaker parks on %s: %w", repoRef.Name, err))
	}
	return errors.Join(errs...)
}

// buildTerminalCircuitBreaker wraps an existing TerminalNotifier with circuit
// breaker logic for PhaseEscalated and PhaseAborted. PhaseFailed is handled by
// buildFailedHandler (which calls applyCircuitBreaker directly), so this
// wrapper skips PhaseFailed to avoid double-counting. Returns nil when no repo
// is configured.
//
// project is the gaggle's own repository (#4243): the failure streak must be
// counted, commented, and parked on the repo this run actually serves, not on
// cfg.Repos[0] — on a multi-repo instance those are different repos whose
// issue numbers collide. Routing matches the sibling terminal hooks, falling
// back to cfg.Repos[0] only when the gaggle declares no project.
//
// Breaker errors are returned, not discarded (#3646): the runner journals a
// TerminalNotifier failure as terminal_notification_failed, so a park that did
// not reach the provider leaves an actionable diagnostic in the run trace
// alongside the durable outbox entry.
func buildTerminalCircuitBreaker(l instance.Layout, cfg *instance.Config, project apiv1.RepoRef, resolver credentials.Resolver, reg runner.SecretRegistrar, inner runner.TerminalNotifier) runner.TerminalNotifier {
	if len(cfg.Repos) == 0 {
		return inner
	}
	poster := &escalationCommenter{
		resolver:           resolver,
		reg:                reg,
		layout:             l,
		needsHumanAssignee: cfg.NeedsHumanAssignee,
	}
	repoRef := terminalRepositoryRefForProject(cfg, project)

	return func(runID string, phase journal.RunPhase, finalState string) error {
		var errs []error
		if phase == journal.PhaseCompleted || phase == journal.PhaseEscalated || phase == journal.PhaseAborted {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			attributedCtx, _ := attributionContextForRun(ctx, l, runID, finalState)
			runURL, _ := failureRunURL(l, cfg, runID)
			if phase == journal.PhaseCompleted {
				if err := resetCircuitBreaker(attributedCtx, poster, l, repoRef, runID, runURL); err != nil {
					errs = append(errs, fmt.Errorf("reset circuit breaker for run %q: %w", runID, err))
				}
			} else if err := applyCircuitBreaker(attributedCtx, poster, l, repoRef, runID, finalState, runURL); err != nil {
				errs = append(errs, fmt.Errorf("apply circuit breaker for run %q: %w", runID, err))
			}
		}
		if inner != nil {
			if err := inner(runID, phase, finalState); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

func withDaemonAttributionTask(ctx context.Context, task string) context.Context {
	attribution, ok := providers.AttributionFromContext(ctx)
	if !ok {
		return ctx
	}
	attribution.Task = task
	attribution.Goober = "runner"
	return providers.WithAttributionContext(ctx, attribution)
}

func attributionContextForRun(ctx context.Context, l instance.Layout, runID, task string) (context.Context, error) {
	reader, err := journal.OpenReadOnly(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		return ctx, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return ctx, err
	}
	return providers.WithAttributionContext(ctx, providers.Attribution{
		Schema: 1, Goobers: true,
		Gaggle: identity.Gaggle, Workflow: identity.Workflow,
		Task: task, Goober: "runner", Run: runID,
	}), nil
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

// buildExistingFixHandler wires runner.Config.ExistingFix (#3236): the
// instance-level handler strips goobers:ready and goobers:critical labels from
// an issue when the implement stage returns no-work with existingFixCommit set,
// preventing a permanent reclaim loop when the fix is already on main.
func buildExistingFixHandler(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar) runner.ExistingFixHandler {
	if len(cfg.Repos) == 0 {
		return nil
	}
	updater := &escalationCommenter{
		resolver:           resolver,
		reg:                reg,
		layout:             l,
		needsHumanAssignee: cfg.NeedsHumanAssignee,
	}

	return func(ctx context.Context, o runner.ExistingFixOutcome) error {
		if o.ItemID == "" {
			return nil
		}
		repoRef := providers.RepositoryRef{
			Provider: providers.ProviderKind(o.RepoRef.Provider),
			Owner:    o.RepoRef.Owner,
			Name:     o.RepoRef.Name,
		}
		// Strip both goobers:ready and goobers:critical labels to prevent reclaim
		_, err := updater.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository:   repoRef,
			ID:           o.ItemID,
			RemoveLabels: []string{providers.LabelReady, providers.LabelCritical},
		})
		return err
	}
}
