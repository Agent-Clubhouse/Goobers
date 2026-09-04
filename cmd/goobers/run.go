package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/hostedprogress"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/signals"
	telemetryingest "github.com/goobers/goobers/internal/telemetry/ingest"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// runPollInterval bounds how often waitForRunTerminal re-reads a run's
// journal while `goobers run` blocks on it. Var, not const, so tests don't
// have to wait out a real 200ms per poll.
var runPollInterval = 200 * time.Millisecond

func exitForPhase(phase journal.RunPhase) int {
	switch phase {
	case journal.PhaseCompleted:
		return 0
	case journal.PhaseFailed, journal.PhaseAborted:
		return 1
	case journal.PhaseEscalated:
		return 3
	default:
		return 1
	}
}

const runHelp = "Usage: goobers run [--gaggle <name>] [--github-progress] [--pr <number>] [--api <url>] [--request-id <id>] <workflow> [--no-wait] [path]\n" +
	"       goobers run <gaggle>/<workflow> [--github-progress] [--pr <number>] [--no-wait] [path]\n" +
	"       goobers run abort <run-id> [path]\n" +
	"       goobers run continue --from <run-id> --terminal-seq <seq> --target <state> --operator <id> [path]\n" +
	"       goobers run cancel <run-id> [path]\n\n" +
	"Trigger a run of a config/ workflow manually, through the same scheduler\n" +
	"(run conditions, instance journal, single-instance lock) a live `goobers up`\n" +
	"daemon uses, then wait for it to reach a terminal state unless\n" +
	"--no-wait is set (default path \".\"). Use --gaggle or the qualified\n" +
	"<gaggle>/<workflow> form when multiple gaggles share a workflow name.\n" +
	"If a live `goobers up` daemon already\n" +
	"holds the instance lock,\n" +
	"delegates the trigger to it instead of failing (#343) — dispatched through\n" +
	"the same Scheduler.Trigger path either way. Exit codes after waiting: 0 =\n" +
	"completed, 1 = failed/aborted or business error (unknown workflow, invalid\n" +
	"config, run conditions rejected the trigger), 2 = usage/IO error, 3 =\n" +
	"escalated. A successful submission-only mode (such as --no-wait, once\n" +
	"available) exits 0 because it does not observe a terminal phase.\n" +
	"--github-progress publishes the versioned hosted-progress contract to one\n" +
	"GitHub Check Run whenever the journal sequence advances. It requires\n" +
	"checks: write plus GITHUB_TOKEN and the standard GitHub Actions environment,\n" +
	"cannot be combined with --no-wait or with --api / $GOOBERS_DAEMON_API\n" +
	"(remote daemon submissions do not publish hosted progress), and does not\n" +
	"replace the final journal artifact.\n" +
	"`run abort` marks a stuck non-terminal run aborted directly in its own\n" +
	"journal — recovery for a run resumeInterruptedRuns can't resolve on its own.\n" +
	"If a live `goobers up` daemon already holds that run's journal lock, abort\n" +
	"delegates to it instead of failing (#2270), the same way `run <workflow>`\n" +
	"delegates triggering (#343) — dispatched through the live-cancel path\n" +
	"either way. `run cancel` instead asks a live daemon to stop a run it is\n" +
	"actively executing (active-stage cancel + worktree/claim teardown +\n" +
	"aborted) — the live counterpart to `run abort`'s daemon-down journal\n" +
	"repair.\n" +
	"With --api (or $GOOBERS_DAEMON_API) the trigger is submitted to that\n" +
	"daemon's authenticated HTTP API instead of the local pending-triggers\n" +
	"drop, so a caller that does not share the daemon's filesystem — CI, a\n" +
	"webhook receiver, another pod — can start a run at all. Nothing local is\n" +
	"read, $GOOBERS_API_TOKEN supplies the bearer token, --request-id makes a\n" +
	"retried submission return the original run instead of minting a second\n" +
	"one, and the command returns once the daemon accepts the trigger because\n" +
	"a remote client cannot watch the run's journal.\n"

func runRun(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "continue" {
		return runRunContinue(args[1:], stdout, stderr)
	}
	fs := newCLIFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noWait := fs.Bool("no-wait", false, "return after the run is dispatched")
	githubProgress := fs.Bool("github-progress", false, "publish live progress to one GitHub Check Run (requires checks: write)")
	gaggle := fs.String("gaggle", "", "trigger the workflow in this gaggle")
	pr := fs.Int("pr", 0, "target pull request (merge-review only)")
	api := fs.String("api", "", "submit the trigger to this daemon API base URL (default $GOOBERS_DAEMON_API)")
	requestID := fs.String("request-id", "", "delivery identity for a retry-safe API submission (default: random)")
	fs.Usage = helpUsage(stderr, "run")
	if err := fs.Parse(runFlagArgs(args)); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	target, err := parseRunTarget(fs.Arg(0), *gaggle)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if *pr < 0 || (*pr == 0 && flagWasSet(args, "pr")) {
		pf(stderr, "error: --pr requires a positive pull request number\n")
		return 2
	}
	target.PR = *pr
	// A configured daemon API endpoint means the daemon is not on this
	// filesystem, so the pending-triggers drop below would land where nothing
	// sweeps it (#3279). Submit through the daemon's trigger plane instead;
	// no instance root is required to ask a remote daemon to act.
	endpoint, err := remoteDaemonAPIBase(*api)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	// --github-progress is a local-run capability: the publisher walks the
	// instance journal on this filesystem to project events, and a remote
	// daemon's journal is not on this filesystem. Reject the combination
	// with the same shape as --github-progress + --no-wait so the flag is
	// never silently ignored on the --api / $GOOBERS_DAEMON_API path
	// (mirrors the earlier no-wait mutex).
	if endpoint != "" && *githubProgress {
		pf(stderr, "error: --github-progress cannot be combined with --api (remote daemon submissions do not publish hosted progress)\n")
		return 2
	}
	if endpoint != "" {
		ctx, stop := signals.SetupSignalContext()
		defer stop()
		return runRemoteTrigger(ctx, endpoint, target, *requestID, *noWait, stdout, stderr)
	}
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}

	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}

	ctx, stop := signals.SetupSignalContext()
	defer stop()
	if *githubProgress {
		if *noWait {
			pf(stderr, "error: --github-progress cannot be combined with --no-wait\n")
			return 2
		}
		if _, err := hostedprogress.Environment(); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		ctx = context.WithValue(ctx, githubProgressContextKey{}, true)
	}

	// Take the same single-instance lock `up` does (issue #134): a manual run
	// must not mutate scheduler/run-condition/claim-ledger state, or the
	// shared workcopies/ tree, concurrently with a live daemon. When a live
	// daemon already holds the lock, delegate through the file-based
	// protocol in rundelegate.go instead of failing (#343 — #231 only fixed
	// the error text; this is the actual behavior fix) rather than requiring
	// the daemon stopped first.
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	release, err := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		return runDelegatedTrigger(ctx, l, target, root, *noWait, stdout, stderr)
	}
	if *noWait && runProcessExits {
		release()
		name := target.String()
		if target.PR > 0 {
			name += "#pr-" + strconv.Itoa(target.PR)
		}
		return runDetachedTrigger(ctx, l, name, root, stdout, stderr)
	}
	return runStandaloneTrigger(ctx, l, target, root, *noWait, false, release, stdout, stderr)
}

type runTarget struct {
	Gaggle   string
	Workflow string
	PR       int
}

func (t runTarget) String() string {
	if t.Gaggle == "" {
		return t.Workflow
	}
	return t.Gaggle + "/" + t.Workflow
}

func parseRunTarget(selector, gaggleFlag string) (runTarget, error) {
	target := runTarget{Gaggle: gaggleFlag, Workflow: selector}
	if strings.Contains(selector, "/") {
		if strings.Count(selector, "/") != 1 {
			return runTarget{}, fmt.Errorf("invalid qualified workflow %q; expected <gaggle>/<workflow>", selector)
		}
		gaggle, workflow, _ := strings.Cut(selector, "/")
		if gaggle == "" || workflow == "" {
			return runTarget{}, fmt.Errorf("invalid qualified workflow %q; expected <gaggle>/<workflow>", selector)
		}
		if gaggleFlag != "" && gaggleFlag != gaggle {
			return runTarget{}, fmt.Errorf("--gaggle %q conflicts with qualified workflow %q", gaggleFlag, selector)
		}
		target.Gaggle = gaggle
		target.Workflow = workflow
	}
	return target, nil
}

// runStandaloneTrigger owns the one-shot scheduler and instance lock. A real
// detached worker stays alive until Starter.Start returns so paused runs
// release those resources; in-process callers hand that cleanup to a goroutine.
func runStandaloneTrigger(ctx context.Context, l instance.Layout, target runTarget, root string, noWait, worker bool, release func(), stdout, stderr io.Writer) (result int) {
	releaseOnReturn := true
	defer func() {
		if releaseOnReturn {
			release()
		}
	}()

	var wg sync.WaitGroup
	// DS6 for the one-shot path (#3512 review, finding 2): this command holds
	// the instance lock, so the daemon — and with it every claim renewal — is
	// stopped. On an engine-configured instance the setup-time reap plus
	// Claim's expired-lease takeover would both fire on a live distributed
	// run's stale-looking lease, so renewal must run before any
	// scheduling/claiming does. Mode-1 gets a nil recovery: byte-identical
	// recover-at-setup behavior.
	claimRecovery := newOneShotClaimRecovery(l)
	setup, err := buildSchedulerSetup(ctx, l, &wg, claimRecovery.setupOptions()...)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if warning := windowsLargeRepoEnvironmentWarning(setup.Config, l.WorkcopiesDir(), realWindowsLargeRepoPreflightDeps()); warning != "" {
		pln(stdout, warning)
	}
	// #3851: a discarded close error here would lose final telemetry, rollup,
	// or journal state without any diagnostic, and — because the issue
	// requires not reporting clean completion after losing final persisted
	// state — without failing the command either. Shutdown itself runs once,
	// so both this defer and the --no-wait cleanup below can call it. When
	// shutdown runs synchronously (every return path except --no-wait, which
	// hands cleanup to a detached goroutine after already returning 0), a
	// failure here downgrades an otherwise-successful result to failure; it
	// never masks a run-outcome exit code that is already non-zero.
	shutdownSetup := func() error {
		if err := setup.Shutdown(context.Background()); err != nil {
			pf(stderr, "error: shut down scheduler services: %v\n", err)
			return err
		}
		return nil
	}
	shutdownOnReturn := true
	defer func() {
		if shutdownOnReturn {
			if err := shutdownSetup(); err != nil && result == 0 {
				result = 1
			}
		}
	}()
	if err := claimRecovery.finish(ctx, l, setup, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	matches := 0
	gaggle := target.Gaggle
	subscribesToPullRequests := false
	for _, e := range setup.Entries {
		if e.Workflow == target.Workflow && (target.Gaggle == "" || e.Gaggle == target.Gaggle) {
			matches++
			if matches == 1 {
				gaggle = e.Gaggle
			}
			for _, signal := range e.Signals {
				if signal == webhookhttp.SignalName("pull_request") {
					subscribesToPullRequests = true
				}
			}
		}
	}
	if matches == 0 {
		if target.Gaggle != "" {
			pf(stderr, "error: no workflow named %q in gaggle %q\n", target.Workflow, target.Gaggle)
		} else {
			pf(stderr, "error: no workflow named %q in %s\n", target.Workflow, l.ConfigDir())
		}
		return 1
	}
	if target.PR > 0 && !subscribesToPullRequests {
		pf(stderr, "error: --pr requires a workflow subscribed to the pull_request event\n")
		return 1
	}

	opts := append(setup.SchedulerOptions(), localscheduler.WithInstanceRunConditions(setup.RunConditions.MaxParallelRuns, setup.RunConditions.WorkflowBudgets, setup.RunConditions.WorkflowDailyBudgets))
	sched := localscheduler.New(setup.Entries, setup.InstanceLog, opts...)
	runDirs, err := l.RunDirs()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := sched.ReconcileAll(runDirs, time.Now()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	triggerCtx := ctx
	if noWait && !worker {
		triggerCtx = context.WithoutCancel(ctx)
	}
	var runID string
	if target.Gaggle != "" || target.PR > 0 {
		identity := localscheduler.WorkflowIdentity{Gaggle: gaggle, Workflow: target.Workflow}
		if target.PR > 0 {
			if target.Gaggle != "" {
				runID, err = sched.TriggerSignalExact(triggerCtx, identity, webhookhttp.SignalName("pull_request"),
					webhookhttp.TriggerRef(webhookhttp.Delivery{Event: "pull_request", PullNumber: target.PR}), time.Now())
			} else {
				runID, err = sched.TriggerSignal(triggerCtx, target.Workflow, webhookhttp.SignalName("pull_request"),
					webhookhttp.TriggerRef(webhookhttp.Delivery{Event: "pull_request", PullNumber: target.PR}), time.Now())
			}
		} else {
			runID, err = sched.TriggerExact(triggerCtx, identity, time.Now())
		}

	} else {
		runID, err = sched.Trigger(triggerCtx, target.Workflow, time.Now())
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "created run %s (workflow=%s gaggle=%s)\n", runID, target.Workflow, gaggle)
	if noWait {
		shutdownOnReturn = false
		releaseOnReturn = false
		cleanup := func() {
			sched.Wait()
			_ = shutdownSetup()
			release()
		}
		pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
		if worker {
			cleanup()
		} else {
			go cleanup()
		}
		return 0
	}

	runDir := filepath.Join(l.ForGaggle(gaggle).RunsDir(), runID)
	reporter := newHostedRunWaitReporter(ctx, runID, runDir, stderr)
	phase, err := waitForRunTerminalWithReporter(ctx, l.ForGaggle(gaggle).RunsDir(), runID, reporter)
	if err != nil {
		reporter.Finalize(err)
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if !isTerminalPhase(phase) {
		// A cancelled wait (Ctrl-C, cancelled Actions job) can return a
		// non-terminal phase with no error; close the Check Run rather than
		// leave it stuck at "in progress" until the workflow job times out.
		reporter.Finalize(ctx.Err())
	}

	// waitForRunTerminal polls the run's OWN journal and returns as soon as
	// it sees a terminal phase — that races trackedStarter.Start's dispatch
	// goroutine, which still has its post-completion telemetry ingest
	// (telemetryingest.RunTelemetry) to run before it calls wg.Done(). Waiting for wg
	// here (this run is the only dispatch `goobers run` ever tracks) closes
	// that gap, so `goobers trace` run immediately afterward reliably sees
	// this run's rollup rows without needing a separate --rebuild.
	wg.Wait()

	pf(stdout, "finished: phase=%s\n", phase)
	pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
	return exitForPhase(phase)
}

func flagWasSet(args []string, name string) bool {
	for _, arg := range args {
		if arg == "--"+name || arg == "-"+name || strings.HasPrefix(arg, "--"+name+"=") || strings.HasPrefix(arg, "-"+name+"=") {
			return true
		}
	}
	return false
}

type targetedPullRequestReader interface {
	GetPullRequest(context.Context, providers.RepositoryRef, string) (providers.PullRequestSummary, error)
}

func validateTargetedPullRequest(ctx context.Context, root string, cfg *instance.Config, stores credentials.StoreResolver, registrar credentials.SecretRegistrar, entry localscheduler.WorkflowEntry, number int) error {
	if number <= 0 {
		return errors.New("pull request number must be a positive integer")
	}
	if cfg == nil {
		return fmt.Errorf("validate pull request #%d: instance configuration is unavailable", number)
	}
	configured, ok := configuredRepoForProject(cfg, entry.RepoRef)
	if !ok {
		return fmt.Errorf("validate pull request #%d: repository %s is not configured in this instance", number, targetedRepoDisplay(entry.RepoRef))
	}
	resolvedRepo := apiv1.RepoRef{
		Provider: apiv1.Provider(configured.Provider),
		BaseURL:  configured.BaseURL,
		Owner:    configured.Owner,
		Project:  configured.Project,
		Name:     configured.Name,
	}
	repo := providers.RepositoryRef{
		Provider: providers.ProviderKind(resolvedRepo.Provider),
		Owner:    resolvedRepo.Owner,
		Project:  resolvedRepo.Project,
		Name:     resolvedRepo.Name,
		URL:      resolvedRepo.BaseURL,
	}
	repoDisplay := targetedRepoDisplay(resolvedRepo)

	var provider providers.Provider
	var err error
	switch repo.Provider {
	case providers.ProviderGitHub, providers.ProviderGitea:
		owner := resolvedRepo.Owner
		credentialCapability := capability.GitHubPRWrite
		if repo.Provider == providers.ProviderGitea {
			credentialCapability = capability.ProviderPRWrite
		}
		resolver, grants, buildErr := buildCredentials(cfg, stores, owner, resolvedRepo.Name, nil, registrar)
		if buildErr != nil {
			return fmt.Errorf("resolve credentials for pull request #%d in %s: %w", number, repoDisplay, buildErr)
		}
		injector, buildErr := credentials.NewInjector(resolver, grants, registrar)
		if buildErr != nil {
			return fmt.Errorf("resolve credentials for pull request #%d in %s: %w", number, repoDisplay, buildErr)
		}
		set, buildErr := injector.Materialize(ctx, []string{string(credentialCapability)})
		if buildErr != nil {
			return fmt.Errorf("not authorized to read pull request #%d in %s: %w", number, repoDisplay, buildErr)
		}
		token, buildErr := set.Token(ctx, string(credentialCapability))
		if buildErr != nil {
			return fmt.Errorf("not authorized to read pull request #%d in %s: %w", number, repoDisplay, buildErr)
		}
		provider, err = newProviderForStage(root, repo, true, withStageProviderToken(token))
	default:
		provider, err = newProviderForStage(root, repo, true)
	}
	if err != nil {
		return fmt.Errorf("validate pull request #%d in configured repository %s: %w", number, repoDisplay, err)
	}
	reader, ok := provider.(targetedPullRequestReader)
	if !ok {
		return fmt.Errorf("validate pull request #%d: provider %q does not support pull-request lookup", number, repo.Provider)
	}
	pr, err := reader.GetPullRequest(ctx, repo, strconv.Itoa(number))
	if err != nil {
		return fmt.Errorf("validate pull request #%d in configured repository %s: %w", number, repoDisplay, err)
	}
	if pr.Number != number {
		return fmt.Errorf("pull request #%d was not returned by the configured repository", number)
	}
	state := strings.TrimSpace(pr.State)
	if !strings.EqualFold(state, "open") {
		return fmt.Errorf("pull request #%d is %s; targeted merge review requires an open pull request", number, state)
	}
	if !pullRequestURLMatchesRepository(resolvedRepo, pr.URL, number) {
		return fmt.Errorf("pull request #%d resolves outside configured repository %s", number, repoDisplay)
	}
	return nil
}

func targetedRepoDisplay(repo apiv1.RepoRef) string {
	if repo.Provider == apiv1.ProviderADO && repo.Project != "" {
		return repo.Owner + "/" + repo.Project + "/" + repo.Name
	}
	return repo.Owner + "/" + repo.Name
}

func pullRequestURLMatchesRepository(repo apiv1.RepoRef, rawURL string, number int) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return false
	}
	parts := strings.FieldsFunc(parsed.EscapedPath(), func(r rune) bool { return r == '/' })
	for i := range parts {
		parts[i], err = url.PathUnescape(parts[i])
		if err != nil {
			return false
		}
	}
	numberText := strconv.Itoa(number)
	switch repo.Provider {
	case apiv1.ProviderGitHub:
		if !strings.EqualFold(parsed.Hostname(), "github.com") {
			return false
		}
		return pullRequestPathMatchesRepository(parts, repo.Owner, repo.Name, numberText)
	case apiv1.ProviderGitea:
		baseURL, baseErr := url.Parse(strings.TrimSpace(repo.BaseURL))
		if baseErr != nil || baseURL.Host == "" || !strings.EqualFold(parsed.Host, baseURL.Host) {
			return false
		}
		return pullRequestPathMatchesRepository(parts, repo.Owner, repo.Name, numberText)
	case apiv1.ProviderADO:
		host := strings.ToLower(parsed.Hostname())
		visualStudioHost := strings.ToLower(repo.Owner) + ".visualstudio.com"
		if host != "dev.azure.com" && host != visualStudioHost {
			return false
		}
		for i := 1; i+3 < len(parts); i++ {
			organizationMatches := host == visualStudioHost ||
				(i >= 2 && strings.EqualFold(parts[i-2], repo.Owner))
			if organizationMatches &&
				strings.EqualFold(parts[i], "_git") &&
				strings.EqualFold(strings.TrimSuffix(parts[i+1], ".git"), repo.Name) &&
				strings.EqualFold(parts[i+2], "pullrequest") &&
				parts[i+3] == numberText {
				return repo.Project == "" || strings.EqualFold(parts[i-1], repo.Project)
			}
		}
	}
	return false
}

func pullRequestPathMatchesRepository(parts []string, owner, name, number string) bool {
	for i := 2; i+1 < len(parts); i++ {
		if (strings.EqualFold(parts[i], "pull") || strings.EqualFold(parts[i], "pulls")) &&
			parts[i+1] == number &&
			strings.EqualFold(parts[i-2], owner) &&
			strings.EqualFold(strings.TrimSuffix(parts[i-1], ".git"), name) {
			return true
		}
	}
	return false
}

// runDelegatedTrigger is #343's actual fix: called when acquireInstanceLock
// finds a live `goobers up` daemon already holding this instance's lock — it
// no longer just reports that and gives up (#231's fix stopped there). It
// writes a delegation request (rundelegate.go) the daemon's own periodic
// sweep picks up and dispatches through the identical Scheduler.Trigger path
// this process would have called itself, then waits for a response and the
// dispatched run's terminal state unless noWait is set. From the caller's
// perspective the two paths are otherwise indistinguishable except for which
// process actually held the scheduler.
func runDelegatedTrigger(ctx context.Context, l instance.Layout, target runTarget, root string, noWait bool, stdout, stderr io.Writer) int {
	var (
		requestID string
		err       error
	)
	if target.PR > 0 {
		requestID, err = writeTargetedTriggerRequestContext(ctx, l.SchedulerDir(), target.Gaggle, target.Workflow, target.PR)
	} else {
		requestID, err = writeTriggerRequestContext(ctx, l.SchedulerDir(), target.Gaggle, target.Workflow)
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	runID, err := pollTriggerResponse(ctx, l.SchedulerDir(), requestID, triggerDelegationTimeout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "created run %s (workflow=%s, dispatched via live daemon)\n", runID, target.Workflow)
	if noWait {
		pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
		return 0
	}

	phase, err := waitForRunTerminalInLayoutWithProgress(ctx, l, runID, stderr)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	pf(stdout, "finished: phase=%s\n", phase)
	pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
	return exitForPhase(phase)
}

// runFlagArgs lets --no-wait appear after the workflow, as documented. The
// standard flag package otherwise stops parsing at the first positional arg.
func runFlagArgs(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--no-wait" || arg == "-no-wait" ||
			strings.HasPrefix(arg, "--no-wait=") || strings.HasPrefix(arg, "-no-wait=") {
			flags = append(flags, arg)
			continue
		}
		if arg == "--github-progress" || arg == "-github-progress" ||
			strings.HasPrefix(arg, "--github-progress=") || strings.HasPrefix(arg, "-github-progress=") {
			flags = append(flags, arg)
			continue
		}
		if arg == "--gaggle" || arg == "-gaggle" {
			flags = append(flags, arg)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if arg == "--pr" || arg == "-pr" ||
			arg == "--api" || arg == "-api" ||
			arg == "--request-id" || arg == "-request-id" {
			flags = append(flags, arg)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if strings.HasPrefix(arg, "--api=") || strings.HasPrefix(arg, "-api=") ||
			strings.HasPrefix(arg, "--request-id=") || strings.HasPrefix(arg, "-request-id=") {
			flags = append(flags, arg)
			continue
		}
		if strings.HasPrefix(arg, "--gaggle=") || strings.HasPrefix(arg, "-gaggle=") {
			flags = append(flags, arg)
			continue
		}
		if strings.HasPrefix(arg, "--pr=") || strings.HasPrefix(arg, "-pr=") {
			flags = append(flags, arg)
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...)
}

// runRunAbort marks a stuck non-terminal run as aborted by appending a
// terminal run.finished(status=aborted) event directly to its own journal —
// issue #135's sanctioned recovery path for a run resumeInterruptedRuns
// can't resolve on its own (e.g. its workflow was renamed/removed from
// config, so `goobers up` skips it with a warning forever rather than
// erroring at startup). It doesn't need the run's workflow or gaggle to still
// exist, but uses the owning gaggle's placement config when available.

const runAbortHelp = "Usage: goobers run abort <run-id> [path]\n\n" +
	"Mark a stuck non-terminal run aborted by appending a terminal\n" +
	"run.finished(status=aborted) event to its own journal (default path\n" +
	"\".\"). An ENGINE-DRIVEN run is cancelled on the engine instead — its\n" +
	"journal is never edited here, and the engine writes its terminal event.\n" +
	"Exit codes: 0 = aborted, 1 = business error (run already terminal),\n" +
	"2 = usage/IO error (unknown run).\n"

func runRunAbort(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("run abort", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "run abort")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	runID := fs.Arg(0)
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}

	l := instance.NewLayout(root)
	runID, err := resolveRunID(l, runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	dir, err := l.FindRunDir(runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	identity, err := reader.Identity()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	// `run abort` appends a terminal event straight into the run's own
	// journal. On an engine-driven run that is a forgery: the workflow keeps
	// executing on the engine and keeps emitting into the journal this
	// command just declared finished. So it does not abort one — it asks the
	// ENGINE to cancel the workflow (#3877), and the engine writes the run's
	// terminal itself. Nothing in this process touches that journal.
	//
	// An already-terminal engine run is NOT routed here — it falls through to
	// the terminal guard below, which answers "run %s is already terminal".
	// There is no workflow left to cancel, and pointing the operator at one
	// that has finished is the same class of misleading answer.
	if identity.EngineDriven() && !engineRunSettledOnDisk(reader) {
		return runEngineDrivenCancel(l, identity, "run abort", stdout, stderr)
	}
	runLayout := l
	if identity.Gaggle != "" && filepath.Clean(filepath.Dir(dir)) != filepath.Clean(l.RunsDir()) {
		runLayout = l.ForGaggle(identity.Gaggle)
	}
	cfg := &instance.Config{}
	if loaded, loadErr := instance.LoadConfig(l.ConfigFile()); loadErr == nil {
		cfg = loaded
	} else if !errors.Is(loadErr, iofs.ErrNotExist) {
		pf(stderr, "warning: load instance config for workcopies placement: %v; continuing with default workcopies layout\n", loadErr)
	}
	if configured, resolveErr := instance.EffectiveWorkcopiesLayout(runLayout, cfg, nil); resolveErr == nil {
		runLayout = configured
	} else {
		pf(stderr, "warning: resolve instance workcopies placement: %v; continuing with default workcopies layout\n", resolveErr)
	}
	workcopiesRoot := runLayout.WorkcopiesDir()
	if runLayout.Gaggle() != "" {
		set, report, loadErr := loadConfigDirectory(l.ConfigDir())
		if loadErr != nil {
			printValidationIssues(stderr, report)
			pf(stderr, "warning: load config directory for workcopies placement: %v; continuing without gaggle placement\n", loadErr)
		} else {
			gaggle := configuredGaggle(set, runLayout.Gaggle())
			if configured, resolveErr := instance.EffectiveWorkcopiesLayout(runLayout, cfg, gaggle); resolveErr == nil {
				runLayout = configured
				workcopiesRoot = runLayout.WorkcopiesDir()
				if gaggle != nil {
					if repo, ok := configuredRepoForProject(cfg, gaggle.Spec.Project); ok && repo.Pinned() {
						workcopiesRoot = runLayout.WorkcopiesBaseDir()
					}
				}
			} else {
				pf(stderr, "warning: resolve gaggle workcopies placement: %v; continuing without gaggle placement\n", resolveErr)
			}
		}
	}
	wtMgr, err := worktree.NewManager(workcopiesRoot)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	if phase, err := reader.Phase(); err == nil {
		// Event-log-first (#242): a stale state.json can still claim
		// {running, ...} after a crash-fsynced run.finished — trusting it
		// here would let abort append a SECOND run.finished onto an
		// already-terminal run, flipping its recorded terminal phase.
		switch phase {
		case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
			if err := finalizeTerminalRunForRecovery(runLayout, nil, wtMgr, runID); err != nil {
				pf(stderr, "error: finalize terminal run %s: %v\n", runID, err)
				return 2
			}
			pf(stderr, "error: run %s is already terminal (phase=%s)\n", runID, phase)
			return 1
		}
	}

	registrar, scrubber := journal.DefaultScrubber()
	run, _, err := journal.Recover(dir, journal.WithScrubber(scrubber))
	if err != nil {
		if errors.Is(err, journal.ErrLockTimeout) {
			if code, handled := delegateAbortToLiveDaemon(l, runID, identity, stdout, stderr); handled {
				return code
			}
		}
		pf(stderr, "error: %v\n", err)
		return 2
	}
	defer func() { _ = run.Close() }()
	if err := prepareAbortedRunBranch(runLayout, runID, run, registrar); err != nil {
		pf(stderr, "warning: terminal branch cleanup for run %s: %v\n", runID, err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted)}); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	// #2191: an aborted run advanced its journal just like a normal terminal
	// run, but unlike the daemon's telemetryingest.RunTelemetry call this one-shot CLI
	// path never opens the intake store — so the dashboard never learns the
	// abort happened until the repair sweep eventually finds it.
	if watermarks, err := intake.Open(runLayout.IntakeDB()); err != nil {
		pf(stderr, "warning: open intake store for run %s: %v\n", runID, err)
	} else {
		telemetryingest.RunIntake(watermarks, runLayout, runID, nil)
		_ = watermarks.Close()
	}
	if err := finalizeTerminalRunForRecovery(runLayout, nil, wtMgr, runID); err != nil {
		pf(stderr, "error: finalize aborted run %s: %v\n", runID, err)
		return 2
	}

	pf(stdout, "aborted run %s\n", runID)
	return 0
}

func configuredGaggle(set *instance.ConfigSet, name string) *apiv1.Gaggle {
	if set == nil {
		return nil
	}
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == name {
			return &set.Gaggles[i]
		}
	}
	return nil
}

// delegateAbortToLiveDaemon is invoked when journal.Recover fails because a
// live `goobers up` daemon already holds the run's journal lock (#2270):
// without this, abort would surface a confusing 30s lock-timeout error
// instead of doing what the operator actually wants. It routes the request
// through the same live-cancel protocol `run cancel` uses, mirroring
// `run <workflow>`'s existing trigger-delegation pattern (#343) — the
// mechanisms stay separate, only the CLI's choice of which one to use
// becomes automatic. handled is false when there turns out to be no live
// daemon after all (a stale/contended lock some other way, or the daemon
// released the run between the failed Recover and this check), in which case
// the caller should fall back to reporting the original journal error
// unchanged.
func delegateAbortToLiveDaemon(l instance.Layout, runID string, identity journal.RunIdentity, stdout, stderr io.Writer) (code int, handled bool) {
	running, _, err := inspectDaemonLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil || !running {
		return 0, false
	}

	requestID, err := writeCancelRequest(l.SchedulerDir(), cancelRequest{
		RunID:    runID,
		Workflow: identity.Workflow,
		Gaggle:   identity.Gaggle,
		Actor:    "cli",
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2, true
	}
	resp, err := pollCancelResponse(context.Background(), l.SchedulerDir(), requestID, cancelDelegationTimeout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2, true
	}
	switch {
	case resp.Error != "":
		pf(stderr, "error: %s\n", resp.Error)
		return 1, true
	case resp.Code == cancelCodeAborted:
		pf(stdout, "aborted run %s (delegated to live daemon)\n", runID)
		return 0, true
	case resp.Code == cancelCodeTerminal:
		pf(stderr, "error: run %s finished before it could be aborted (phase=%s)\n", runID, resp.Phase)
		return 1, true
	case resp.Code == cancelCodeNotRunning:
		// The daemon was live when we inspected the lock but no longer owns
		// this run by the time the sweep picked up the request — fall back to
		// the original journal error rather than claiming success.
		return 0, false
	default:
		pf(stderr, "error: unexpected cancel response for run %s\n", runID)
		return 1, true
	}
}

const runCancelHelp = "Usage: goobers run cancel <run-id> [path]\n\n" +
	"Ask the live `goobers up` daemon to stop a run it is actively executing\n" +
	"(default path \".\"): it cancels the active stage, tears down the run\n" +
	"worktree, releases the backlog claim so the item can be re-queued, and\n" +
	"records terminal phase aborted — without stopping the daemon or editing a\n" +
	"journal behind its back. An ENGINE-DRIVEN run is cancelled on the engine\n" +
	"(CancelWorkflow) instead, with no live daemon required. Use `run abort`\n" +
	"instead when no daemon is running (that path finalizes a stuck run's\n" +
	"journal directly). Exit codes: 0 = cancelled, 1 = business error\n" +
	"(already terminal, not currently running, or no daemon to cancel it),\n" +
	"2 = usage/IO error (unknown run).\n"

func runRunCancel(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("run cancel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "run cancel")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	runID := fs.Arg(0)
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}

	l := instance.NewLayout(root)
	runID, err := resolveRunID(l, runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	dir, err := l.FindRunDir(runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	identity, err := reader.Identity()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	// `run cancel` asks the daemon to stop a run it is executing in-process.
	// It never is for an engine-driven run — so the cancellation goes to the
	// ENGINE instead (#3877), where the run actually executes. This is
	// deliberately NOT gated on a live daemon: the daemon is not the thing
	// driving the run, and requiring one would leave an engine run
	// unstoppable during exactly the outage an operator most wants to stop it
	// in. As with abort, an already-terminal run gets the accurate "already
	// terminal" answer from the guard below instead.
	if identity.EngineDriven() && !engineRunSettledOnDisk(reader) {
		return runEngineDrivenCancel(l, identity, "run cancel", stdout, stderr)
	}
	// Event-log-first terminal guard (#242), matching `run abort`: a run that
	// already finished has nothing live to cancel.
	if phase, err := reader.Phase(); err == nil {
		switch phase {
		case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
			pf(stderr, "error: run %s is already terminal (phase=%s)\n", runID, phase)
			return 1
		}
	}

	// A cancel is inherently a live operation: without a running daemon there is
	// no in-flight run to stop. Point the operator at the offline repair path
	// rather than silently doing nothing (or racing a daemon that just exited).
	running, _, err := inspectDaemonLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if !running {
		pf(stderr, "error: no `goobers up` daemon is running, so run %s is not executing; "+
			"use `goobers run abort %s` to finalize a stuck run's journal\n", runID, runID)
		return 1
	}

	requestID, err := writeCancelRequest(l.SchedulerDir(), cancelRequest{
		RunID:    runID,
		Workflow: identity.Workflow,
		Gaggle:   identity.Gaggle,
		Actor:    "cli",
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	resp, err := pollCancelResponse(context.Background(), l.SchedulerDir(), requestID, cancelDelegationTimeout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	switch {
	case resp.Error != "":
		pf(stderr, "error: %s\n", resp.Error)
		return 1
	case resp.Code == cancelCodeAborted:
		pf(stdout, "cancelled run %s (aborted)\n", runID)
		return 0
	case resp.Code == cancelCodeTerminal:
		pf(stderr, "error: run %s finished before it could be cancelled (phase=%s)\n", runID, resp.Phase)
		return 1
	case resp.Code == cancelCodeNotRunning:
		pf(stderr, "error: run %s is not currently running under the daemon\n", runID)
		return 1
	default:
		pf(stderr, "error: unexpected cancel response for run %s\n", runID)
		return 1
	}
}

// waitForRunTerminal polls runID's journal until it reaches a terminal phase
// or ctx is cancelled. Scheduler.Trigger's own dispatch continues
// asynchronously in a background goroutine (issue #134) — this is what
// preserves `goobers run`'s existing block-until-done UX rather than
// returning the instant the run is merely admitted. A run that pauses at a
// human gate (or a daemon-drain checkpoint, though none applies here since
// `run` holds its own instance lock) stays PhaseRunning indefinitely by
// design; ctx cancellation (SIGINT/SIGTERM) is what lets a caller stop
// waiting on it, reporting its phase as of that moment.
// runTerminalWaitTimeout, when > 0, bounds how long waitForRunTerminal polls
// WITHOUT OBSERVING PROGRESS before giving up with an error. It is 0
// (unbounded) in production: a human running `goobers run` waits until the run
// finishes or they Ctrl-C, and nothing should cut that short. The test suite
// sets a generous bound (see cmd/goobers TestMain) so that if the
// concurrent-make-ci journal-IO wedge (#827) ever regresses, the affected test
// FAILS FAST — in ~2 minutes — instead of silently hanging the whole local-ci
// stage for its full 10-minute limit and wedging the merge queue with no signal.
// Var, not const, so only the suite opts in; production leaves it 0.
//
// The bound is idle time, not total elapsed time, because a wedge is the
// ABSENCE of journal progress, not slowness. Bounding total elapsed time made
// the tripwire fire on runs that were demonstrably healthy: under a saturated
// concurrent `make ci`, TestDemoTourRunsOfflineThroughDaemon's nested run kept
// advancing stage by stage (curate at 12s, implement at 1m39s, review at 1m53s)
// and was failed at the 2-minute mark purely for being slow — a false red that
// costs a whole CI repass and teaches nothing. Resetting the deadline on every
// newly observed journal event keeps a genuinely wedged run failing just as
// fast (a wedge appends nothing, so its idle clock never resets) while a
// merely-slow-under-load run runs to completion.
var runTerminalWaitTimeout time.Duration

type githubProgressContextKey struct{}

func githubProgressEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(githubProgressContextKey{}).(bool)
	return enabled
}

// isTerminalPhase reports whether a run has reached one of the four terminal
// phases waitForRunTerminal waits for.
func isTerminalPhase(p journal.RunPhase) bool {
	switch p {
	case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
		return true
	}
	return false
}

func waitForRunTerminal(ctx context.Context, runsDir, runID string) (journal.RunPhase, error) {
	return waitForRunTerminalWithReporter(ctx, runsDir, runID, newRunWaitReporter(runID, io.Discard))
}

func waitForRunTerminalWithReporter(ctx context.Context, runsDir, runID string, progress *runWaitReporter) (journal.RunPhase, error) {
	dir := filepath.Join(runsDir, runID)
	observedEvents := -1
	lastProgress := time.Now()
	for {
		if reader, err := journal.OpenRead(dir); err == nil {
			events, eventsErr := reader.Events()
			if eventsErr != nil {
				return journal.PhaseRunning, fmt.Errorf("read progress for run %s: %w", runID, eventsErr)
			}
			if len(events) != observedEvents {
				observedEvents = len(events)
				lastProgress = time.Now()
			}
			progress.observe(events, time.Now())
			// Terminality is decided from the very slice just rendered, not by
			// re-reading through runPhase. The events file is still being
			// appended to, so a second read can see the terminal event that
			// this one missed, and returning on it would drop the final
			// stage's "finished" line from user-visible progress (#1557).
			if phase := journal.PhaseFromEvents(events); isTerminalPhase(phase) {
				return phase, nil
			}
		} else if errors.Is(err, journal.ErrNotRunDirectory) {
			progress.heartbeat(time.Now())
		} else {
			return journal.PhaseRunning, fmt.Errorf("open run %s while waiting for terminal phase: %w", runID, err)
		}

		// An idle bound (only ever set by the test suite) elapsing on a
		// still-running run is the #827-regression tripwire: surface it as an
		// error so the caller exits non-zero and the test fails fast, rather
		// than reporting a non-terminal phase as though the wait completed
		// normally.
		if idle := time.Since(lastProgress); runTerminalWaitTimeout > 0 && idle >= runTerminalWaitTimeout {
			phase := journal.PhaseRunning
			if reader, err := journal.OpenRead(dir); err == nil {
				phase = runPhase(reader)
			} else if !errors.Is(err, journal.ErrNotRunDirectory) {
				return journal.PhaseRunning, fmt.Errorf("open run %s after wait timeout: %w", runID, err)
			}
			if isTerminalPhase(phase) {
				return phase, nil
			}
			return phase, fmt.Errorf("run %s did not reach a terminal phase and made no journal progress for %s (still %s); failing fast instead of hanging — a make-ci journal-IO wedge may have regressed (#827)", runID, runTerminalWaitTimeout, phase)
		}

		select {
		case <-ctx.Done():
			if reader, err := journal.OpenRead(dir); err == nil {
				// A signal-driven cancel (production Ctrl-C) reports whatever
				// phase we can read, with no error.
				return runPhase(reader), nil
			} else if !errors.Is(err, journal.ErrNotRunDirectory) {
				return journal.PhaseRunning, fmt.Errorf("open run %s after wait cancellation: %w", runID, err)
			}
			return journal.PhaseRunning, ctx.Err()
		case <-time.After(runPollInterval):
		}
	}
}

func waitForRunTerminalInLayoutWithProgress(ctx context.Context, layout instance.Layout, runID string, progress io.Writer) (journal.RunPhase, error) {
	reporter := newRunWaitReporter(runID, progress)
	for {
		dir, err := layout.FindRunDir(runID)
		if err == nil {
			if githubProgressEnabled(ctx) {
				reporter = newHostedRunWaitReporter(ctx, runID, dir, progress)
			}
			phase, waitErr := waitForRunTerminalWithReporter(ctx, filepath.Dir(dir), runID, reporter)
			if waitErr != nil || !isTerminalPhase(phase) {
				// Close any hosted-progress Check Run on abnormal exit so
				// the workflow job's remote view does not linger at
				// "in progress" past the run's actual end.
				reporter.Finalize(waitErr)
			}
			return phase, waitErr
		}
		if !errors.Is(err, iofs.ErrNotExist) {
			return journal.PhaseRunning, err
		}
		reporter.heartbeat(time.Now())
		select {
		case <-ctx.Done():
			return journal.PhaseRunning, ctx.Err()
		case <-time.After(runPollInterval):
		}
	}
}
