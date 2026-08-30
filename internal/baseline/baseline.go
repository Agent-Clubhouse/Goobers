// Package baseline tells a failing deterministic CI stage apart from a target
// branch that was already failing before the run branched (#2971).
//
// Without it every unrelated branch that syncs a red base independently pays
// for the same failure: an implementation repass that can only re-derive the
// identical diff, a remediation budget spent on a defect the pull request
// never introduced, and — when the agent correctly reports nothing to fix — a
// terminal escalation on an empty diff. The classification here compares the
// observed failure against the SAME command run on the target branch at the
// pinned base SHA, caches that observation so only the first affected run pays
// for it, and records one durable shared blocker every subsequent affected run
// parks against.
package baseline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/flake"
)

// Class is how a failing CI command is attributed.
type Class string

const (
	// ClassPRIntroduced attributes the failure to the branch under test: the
	// baseline for the same command at the pinned base SHA is green, or fails
	// with a different signature.
	ClassPRIntroduced Class = "pr-introduced"
	// ClassSharedBaselineFailure attributes the failure to the target branch:
	// the identical command fails identically at the pinned base SHA, so no
	// branch-local change can fix it.
	ClassSharedBaselineFailure Class = "shared-baseline-failure"
	// ClassUnknown means the baseline is not knowable here — no pinned base
	// SHA, no cached observation and no configured prober. It is deliberately
	// distinct from ClassPRIntroduced so a caller never parks (or blames) a
	// pull request on absent evidence; the caller's pre-existing routing
	// applies unchanged.
	ClassUnknown Class = "unknown"
)

// Request is one failing CI observation to classify.
type Request struct {
	// Repo identifies the target repository ("owner/name"); it namespaces both
	// the baseline cache and the shared blocker.
	Repo string
	// RepoURL is the git remote a baseline probe clones the base commit from.
	// Empty leaves a configured prober to resolve it however it can.
	RepoURL string
	// BaseSHA is the pinned commit of the target branch this run synced. A
	// baseline observation is only ever reused for the exact same commit.
	BaseSHA string
	// Command is the CI command that failed, argv-style.
	Command []string
	// FailureText is the stage's bounded failure evidence (summary, error
	// message, captured diagnostic) the signature is derived from.
	FailureText string
	// RunID and Waiter identify who is waiting on a shared blocker: the run,
	// and the durable subject (backlog item or pull request) to release when
	// the baseline recovers. Both may be empty for a classification-only call.
	RunID  string
	Waiter string
}

// Decision is the classification of one Request.
type Decision struct {
	Class Class
	// BaseSHA and Fingerprint identify what was compared, for journaling.
	BaseSHA     string
	Fingerprint string
	Signature   string
	// BlockerKey names the durable shared blocker this failure belongs to,
	// set only for ClassSharedBaselineFailure.
	BlockerKey string
	// Waiting is how many distinct subjects are parked on that blocker,
	// including this one.
	Waiting int
	// Park reports that the caller should park this run against BlockerKey
	// instead of spending a repass on it. It is true for every shared baseline
	// failure unless the shared repair lane is explicitly configured on.
	Park bool
	// Reason is a human-facing explanation for the journal and any parking
	// comment.
	Reason string
}

// ProbeResult is one execution of the CI command against the target branch at
// a pinned base SHA.
type ProbeResult struct {
	// Green reports that the command succeeded on the untouched base.
	Green bool
	// Output is the failure evidence when Green is false.
	Output string
}

// Prober runs a command against the target branch at a pinned base SHA. It is
// the only part of classification that touches a repository, kept behind this
// seam so the decision logic stays pure and unit-testable.
type Prober interface {
	Probe(ctx context.Context, target ProbeTarget, command []string) (ProbeResult, error)
}

// ProbeTarget names the repository and commit a baseline is measured at.
type ProbeTarget struct {
	Repo    string
	RepoURL string
	BaseSHA string
}

// Evaluator classifies failing CI stages against cached, base-pinned baselines.
// A zero Evaluator is not usable; construct one with a Store.
type Evaluator struct {
	// Store holds baseline observations and shared blockers. Required.
	Store *Store
	// Prober measures a baseline the Store has not observed yet. Optional: with
	// no prober, a request with no cached observation classifies as
	// ClassUnknown rather than guessing.
	Prober Prober
	// ProbeTimeout bounds one baseline measurement. Zero leaves the caller's
	// context deadline as the only bound.
	ProbeTimeout time.Duration
	// RepairLane opts this instance into repairing shared baseline failures on
	// the affected branch instead of parking it. Off by default: silently
	// adding an unrelated repair to every feature branch is exactly the churn
	// this package exists to stop, so it is an explicit configuration choice.
	RepairLane bool
	// Now is the clock, for tests. Defaults to time.Now.
	Now func() time.Time
}

// ErrNoStore reports an Evaluator used without a Store.
var ErrNoStore = errors.New("baseline: evaluator requires a store")

// Classify attributes req's failure to the branch or to its base, recording
// the baseline observation and, for a shared failure, parking req's waiter on
// the durable shared blocker.
func (e *Evaluator) Classify(ctx context.Context, req Request) (Decision, error) {
	if e == nil || e.Store == nil {
		return Decision{}, ErrNoStore
	}
	signature := flake.NormalizeSignature(req.FailureText)
	fingerprint := Fingerprint(req.Command, signature)
	decision := Decision{Class: ClassUnknown, BaseSHA: req.BaseSHA, Fingerprint: fingerprint, Signature: signature}
	if strings.TrimSpace(req.BaseSHA) == "" || len(req.Command) == 0 {
		decision.Reason = "no pinned base SHA or command to compare against"
		return decision, nil
	}

	observation, ok := e.Store.Baseline(req.Repo, req.BaseSHA, req.Command)
	if !ok {
		if e.Prober == nil {
			decision.Reason = "no cached baseline for this base SHA and no prober configured"
			return decision, nil
		}
		measured, err := e.probe(ctx, req)
		if err != nil {
			return decision, err
		}
		observation = measured
	}

	if observation.Green {
		decision.Class = ClassPRIntroduced
		decision.Reason = fmt.Sprintf("baseline %s is green for this command", short(req.BaseSHA))
		return decision, nil
	}
	if observation.Fingerprint != fingerprint {
		decision.Class = ClassPRIntroduced
		decision.Reason = fmt.Sprintf("baseline %s fails with a different signature", short(req.BaseSHA))
		return decision, nil
	}

	decision.Class = ClassSharedBaselineFailure
	decision.Park = !e.RepairLane
	blocker, err := e.Store.Park(observation, Waiter{
		Subject:  req.Waiter,
		RunID:    req.RunID,
		BaseSHA:  req.BaseSHA,
		ParkedAt: e.now(),
	})
	if err != nil {
		return decision, err
	}
	decision.BlockerKey = blocker.Key
	decision.Waiting = len(blocker.Waiting)
	decision.Reason = fmt.Sprintf(
		"identical failure on the target branch at base %s (%s); shared blocker %s has %d waiting subject(s)",
		short(req.BaseSHA), observation.Signature, blocker.Key, decision.Waiting)
	if !decision.Park {
		decision.Reason += "; the shared repair lane is enabled, so this branch may carry the repair"
	}
	return decision, nil
}

func (e *Evaluator) probe(ctx context.Context, req Request) (Observation, error) {
	if e.ProbeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.ProbeTimeout)
		defer cancel()
	}
	result, err := e.Prober.Probe(ctx, ProbeTarget{Repo: req.Repo, RepoURL: req.RepoURL, BaseSHA: req.BaseSHA}, req.Command)
	if err != nil {
		return Observation{}, fmt.Errorf("baseline: probe %q at %s: %w", CommandKey(req.Command), short(req.BaseSHA), err)
	}
	observation := Observation{
		Repo:       req.Repo,
		BaseSHA:    req.BaseSHA,
		Command:    CommandKey(req.Command),
		Green:      result.Green,
		ObservedAt: e.now(),
	}
	if !result.Green {
		observation.Signature = flake.NormalizeSignature(result.Output)
		observation.Fingerprint = Fingerprint(req.Command, observation.Signature)
	}
	if err := e.Store.Record(observation); err != nil {
		return Observation{}, err
	}
	return observation, nil
}

func (e *Evaluator) now() time.Time {
	if e == nil || e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}

// CommandKey renders an argv as the stable string the baseline cache keys on.
func CommandKey(command []string) string {
	return strings.Join(command, "\x1f")
}

// Fingerprint is the identity two failures must share to be the same failure:
// the command that produced them plus their volatility-normalized signature.
func Fingerprint(command []string, signature string) string {
	return flake.Fingerprint(CommandKey(command), "", signature)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
