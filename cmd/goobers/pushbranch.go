package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/providers"
)

// runPushBranch implements `goobers push-branch` (#237): the deterministic
// push stage a workflow declares between local-ci and open-pr, closing the
// gap where an implementer's commits never reached origin — open-pr's PR
// creation would then 422 on a branch that was never pushed, with the
// diagnosis invisible from the journal.
//
// Unlike open-pr/backlog-query/issue-close-out (which talk to a provider's
// REST API), push-branch's target is the worktree's own git remote. GitHub uses
// the runner-injected repo:push token. ADO matches that remote against
// instance.yaml and resolves its configured PAT or Entra credential source.
const pushBranchHelp = "Usage: goobers push-branch [path]\n\n" +
	"Push the worktree's checked-out branch to origin, authenticated via the\n" +
	"configured repository credential — never the host's ambient git\n" +
	"credentials, and never persisted to .git/config.\n" +
	"A push rejected as a ref race (non-fast-forward, \"failed to push some\n" +
	"refs\") fetches the remote tip, rebases the local branch onto it, and\n" +
	"retries up to 2 more times before failing, so a fully-validated diff is\n" +
	"not discarded because a concurrent writer advanced the branch (#3366).\n" +
	"[path] defaults to the current directory (the stage's worktree).\n" +
	"Exit codes: 0 = pushed, 1 = business error, 2 = usage/IO error.\n"

func runPushBranch(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("push-branch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "push-branch")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}

	branch, err := currentBranch(dir)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	env, err := pushBranchEnvironment(dir)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// NOTHING TO PUSH is not the same as a successful push, and the difference
	// is load-bearing. A branch with no commits beyond its base pushes cleanly:
	// git exits 0, origin gains a ref identical to base, and the stage reports
	// success having shipped nothing.
	//
	// The fact recorded below is what makes that dangerous rather than merely
	// useless. #3366's re-claim discovery reads a journaled branch push as
	// "this run did not strand its diff" — so an empty push actively asserts
	// that no work was lost, which is exactly wrong in the case where work WAS
	// lost. That is how a stranded diff becomes unrecoverable instead of
	// recoverable. MEASURED as reachable in mode 3 (#3763): a commit made by
	// one stage does not survive into the next, so the pushing stage genuinely
	// arrives with nothing.
	//
	// DELIBERATELY NOT A FAILURE. This cannot distinguish "the agent correctly
	// made no changes" from "the diff was stranded" — both arrive here with an
	// empty branch — and failing the stage would regress the first case on
	// substrates where it is legitimate. So the stage still succeeds; what it
	// stops doing is claiming a push that did not happen.
	empty, emptyErr := branchHasNoCommitsBeyondBase(dir, branch)
	if emptyErr != nil {
		// Cannot tell: preserve today's behaviour exactly rather than guess.
		pf(stderr, "warning: could not determine whether %q has commits to push (%v); pushing anyway\n", branch, emptyErr)
	} else if empty {
		pf(stderr, "warning: branch %q has no commits beyond its base — nothing to push\n", branch)
		pf(stdout, "no commits to push on %s; skipped (no branch-push fact recorded)\n", branch)
		return 0
	}

	if err := pushBranchWithRetry(dir, branch, env, stderr); err != nil {
		pf(stderr, "error: push branch %q: %v\n", branch, err)
		return 1
	}
	// Record the successful publication in the stage's mutation sidecar so the
	// runner journals it as a ref.touched event (#228's machinery). #3366's
	// re-claim discovery reads it back: a prior run whose journal shows a
	// pushed branch did not strand its diff, so gather-implement-context must
	// not offer that run's work as recoverable.
	appendBranchPushFact(dir, branch)

	pf(stdout, "pushed %s to origin\n", branch)
	return 0
}

// pushRaceAttempts bounds the push → fetch-and-rebase → retry loop: the total
// number of pushes attempted before a persistently rejected push fails the
// stage.
const pushRaceAttempts = 3

// pushBranchWithRetry pushes branch, and when the push is rejected as a ref
// race (#3366's trigger class 3: a concurrent writer advanced the remote
// branch between this worktree's fork point and its push — "failed to push
// some refs" after minutes of validated implement work), fetches the remote
// tip, rebases the local branch onto it, and retries. A rebase that does not
// apply cleanly aborts and surfaces the original push rejection: conflict
// resolution is agentic work, not a push-layer concern. Any non-race failure
// (auth, missing remote) fails immediately, exactly as before.
func pushBranchWithRetry(dir, branch string, env []string, stderr io.Writer) error {
	var err error
	for attempt := 1; ; attempt++ {
		err = gitPushBranch(dir, branch, env)
		if err == nil {
			return nil
		}
		if attempt >= pushRaceAttempts || !isPushRaceError(err) {
			return err
		}
		pf(stderr, "warning: push attempt %d rejected as a ref race; rebasing onto the remote tip and retrying: %v\n", attempt, err)
		if rebaseErr := rebaseOntoRemoteBranch(dir, branch, env); rebaseErr != nil {
			pf(stderr, "warning: rebase onto remote %q failed (%v); surfacing the original push rejection\n", branch, rebaseErr)
			return err
		}
	}
}

// isPushRaceError classifies a push failure as a ref race worth a
// fetch-rebase-retry, from git's own stable rejection phrasing. Everything
// else (auth failures, unreachable remotes, missing refs) is not retryable
// at this layer.
func isPushRaceError(err error) bool {
	msg := err.Error()
	for _, marker := range []string{
		"failed to push some refs",
		"fetch first",
		"non-fast-forward",
		"cannot lock ref",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// rebaseOntoRemoteBranch fetches branch's current remote tip and rebases the
// local branch onto it, authenticated via the same env-injected credential as
// the push. On a rebase failure the rebase is aborted so the worktree is left
// exactly as it was before the attempt.
func rebaseOntoRemoteBranch(dir, branch string, env []string) error {
	url, err := originURL(dir)
	if err != nil {
		return err
	}
	fetch := exec.Command("git", "fetch", url, "refs/heads/"+branch)
	fetch.Dir = dir
	fetch.Env = env
	if out, err := fetch.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch remote tip of %q: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	rebase := exec.Command("git", "rebase", "FETCH_HEAD")
	rebase.Dir = dir
	rebase.Env = env
	if out, err := rebase.CombinedOutput(); err != nil {
		abort := exec.Command("git", "rebase", "--abort")
		abort.Dir = dir
		abort.Env = env
		_ = abort.Run()
		return fmt.Errorf("rebase onto remote tip of %q: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// appendBranchPushFact appends a branch-publication mutation fact to the
// stage worktree's mutation sidecar (mutations.jsonl), best-effort like
// sidecarMutationRecorder: the push already happened for real, so a failed
// sidecar write must never fail the stage — it only costs the ref.touched
// journal projection.
func appendBranchPushFact(dir, branch string) {
	fact := mutationFact{Provider: "git", Kind: "branch", ID: branch, Operation: "push"}
	data, err := json.Marshal(fact)
	if err != nil {
		log.Printf("mutation sidecar: marshal branch push fact: %v", err)
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, mutationsSidecarFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("mutation sidecar: open %s: %v", mutationsSidecarFile, err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(data, '\n')); err != nil {
		log.Printf("mutation sidecar: write %s: %v", mutationsSidecarFile, err)
	}
}

// currentBranch returns the branch checked out at dir — the run branch
// worktree.Manager.Create already created or checked out before this stage's
// process started. push-branch pushes exactly that branch rather than
// reconstructing a name from GOOBERS_RUN_ID/GOOBERS_WORKFLOW, so it can never
// drift from what the worktree actually has checked out.
func currentBranch(dir string) (string, error) {
	cmd := exec.Command("git", "symbolic-ref", "--short", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("determine checked-out branch (detached HEAD?): %w", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("worktree at %s has no checked-out branch (detached HEAD)", dir)
	}
	return branch, nil
}

// branchHasNoCommitsBeyondBase reports whether branch carries nothing origin's
// base branch does not already have.
//
// The base is read from the run context the executor injects, defaulting to
// "main" exactly as the in-pod checkout does, so both substrates agree on what
// "base" means. When the base ref is not present locally — a worktree that
// never fetched it, an unusual remote layout — this returns an error rather
// than a verdict: the caller then preserves the pre-existing behaviour instead
// of skipping a push it cannot prove is empty. Refusing to guess is the point;
// a wrong "empty" verdict would silently drop a real diff, which is worse than
// the problem being fixed.
func branchHasNoCommitsBeyondBase(dir, branch string) (bool, error) {
	base := strings.TrimSpace(os.Getenv(executor.BaseBranchEnvVar))
	if base == "" {
		base = "main"
	}
	// Two substrates store the base under different refs and BOTH must resolve,
	// or the check silently degrades to "cannot tell" on one of them. A pod's
	// `git clone --branch <base>` yields a remote-tracking origin/<base>; the
	// worker's worktrees come off a `git clone --mirror`, which has no
	// refs/remotes/* at all and carries the base at refs/heads/<base>.
	// MEASURED: origin/<base> alone made this permanently undecidable on the
	// worker, which is the substrate where the check matters most today.
	baseRef := ""
	for _, candidate := range []string{"origin/" + base, "refs/heads/" + base, base} {
		verify := exec.Command("git", "rev-parse", "--verify", "--quiet", candidate+"^{commit}")
		verify.Dir = dir
		if err := verify.Run(); err == nil {
			baseRef = candidate
			break
		}
	}
	if baseRef == "" {
		return false, fmt.Errorf("base %q not present locally as origin/%s, refs/heads/%s, or %s", base, base, base, base)
	}
	count := exec.Command("git", "rev-list", "--count", baseRef+".."+branch)
	count.Dir = dir
	out, err := count.Output()
	if err != nil {
		return false, fmt.Errorf("count commits on %s beyond %s: %w", branch, baseRef, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return false, fmt.Errorf("parse commit count %q: %w", strings.TrimSpace(string(out)), err)
	}
	return n == 0, nil
}

// gitPushBranch pushes branch to origin, authenticated via gitAuthEnv (#237's
// "token never lands on disk" requirement).
//
// Pushes to origin's resolved URL, not "origin" by name: worktree.Manager's
// managed working copy is a `git clone --mirror`, which sets
// remote.origin.mirror=true — a worktree checked out off it shares that same
// repo config, and git refuses to combine a mirrored remote with an explicit
// refspec ("fatal: --mirror can't be combined with refspecs"). Pushing by
// URL bypasses that remote-name-keyed restriction entirely.
func gitPushBranch(dir, branch string, env []string) error {
	url, err := originURL(dir)
	if err != nil {
		return err
	}
	cmd := exec.Command("git", "push", url, branch+":"+branch)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func pushBranchEnvironment(dir string) ([]string, error) {
	root := os.Getenv("GOOBERS_INSTANCE_ROOT")
	if root != "" {
		cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
		if err != nil {
			return nil, fmt.Errorf("load instance for repository authentication: %w", err)
		}
		remote, err := originURL(dir)
		if err != nil {
			return nil, err
		}
		if repo, ok := adoRepoForOrigin(cfg, remote); ok {
			// One-shot command scope: push-branch runs as its own process, so
			// it builds its own store registry (#683) for a store-backed PAT.
			stores, err := secretstore.NewRegistry(cfg.SecretStores)
			if err != nil {
				return nil, err
			}
			source, err := adoauth.Source(repo, nil, stores)
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
			defer cancel()
			return providers.ADOGitAuthEnvironment(ctx, source, nil, remote)
		}
		if isADORemote(remote) {
			return nil, fmt.Errorf("ADO origin %q does not match any configured repository", remote)
		}
	}
	token, err := providerToken(capability.RepoPush)
	if err != nil {
		return nil, err
	}
	return gitAuthEnv(token), nil
}

func adoRepoForOrigin(cfg *instance.Config, remote string) (instance.RepoRef, bool) {
	if cfg == nil {
		return instance.RepoRef{}, false
	}
	normalized := strings.TrimSuffix(strings.TrimRight(remote, "/"), ".git")
	for i := range cfg.Repos {
		repo := cfg.Repos[i]
		if repo.Provider != "ado" {
			continue
		}
		expected := fmt.Sprintf("https://dev.azure.com/%s/%s/_git/%s",
			url.PathEscape(repo.Owner), url.PathEscape(repo.Project), url.PathEscape(repo.Name))
		if strings.EqualFold(normalized, expected) {
			return repo, true
		}
	}
	return instance.RepoRef{}, false
}

func isADORemote(remote string) bool {
	parsed, err := url.Parse(remote)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "dev.azure.com" || strings.HasSuffix(host, ".visualstudio.com")
}

// gitAuthEnv builds the env-var-injected git credential (shared by every git
// subprocess this binary spawns against origin: gitPushBranch,
// checkoutExistingBranch, attemptRebase, forcePushWithLease) — per-invocation
// via GIT_CONFIG_COUNT/GIT_CONFIG_KEY_0/GIT_CONFIG_VALUE_0 (git 2.31+'s
// environment-based config), never a URL-embedded credential or a
// command-line -c flag, so the token never appears in argv (visible via
// `ps`) and is never written to any file. GitHub's HTTPS token convention is
// basic auth with the token as the password and any non-empty username;
// "x-access-token" is GitHub's own documented placeholder for that username.
func gitAuthEnv(token string) []string {
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+auth,
	)
}

// originURL resolves the worktree's "origin" remote to its configured URL.
func originURL(dir string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve origin URL: %w", err)
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("worktree at %s has no origin remote configured", dir)
	}
	return url, nil
}
