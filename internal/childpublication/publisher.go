// Package childpublication publishes an explicitly delegated child result from
// its host-owned managed fork. Model processes never receive provider tokens.
package childpublication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

// EffectTimeout bounds one publication attempt while current authority is held.
const EffectTimeout = 30 * time.Second

// Target is immutable source admission, never an authored repository override.
type Target struct {
	Child      triggerqueue.ChildRecord
	Identity   journal.RunIdentity
	Repository providers.RepositoryRef
	Remote     string
	Base       string
	Head       string
	Stage      string
	Workspace  string
	Fork       recovery.ChildSnapshot
}

// BranchIntent pins the target and immutable ancestor-preserving snapshot.
type BranchIntent struct {
	Version    int                     `json:"version"`
	RunID      string                  `json:"runId"`
	Lineage    journal.ChildLineage    `json:"lineage"`
	Stage      string                  `json:"stage"`
	Repository providers.RepositoryRef `json:"repository"`
	Remote     string                  `json:"remote"`
	Base       string                  `json:"base"`
	Head       string                  `json:"head"`
	Snapshot   recovery.ChildSnapshot  `json:"snapshot"`
	Commit     string                  `json:"commit"`
}

// PRIntent pins one request to its confirmed branch intent.
type PRIntent struct {
	Version      int                          `json:"version"`
	BranchDigest string                       `json:"branchDigest"`
	Request      providers.PullRequestRequest `json:"request"`
}

// BranchReceipt records the exact remote reference observed after an effect.
type BranchReceipt struct {
	Head string `json:"head"`
	SHA  string `json:"sha"`
}

// PRProvider uses the existing native reconciliation surface for exact head/base.
type PRProvider interface {
	OpenPullRequest(context.Context, providers.PullRequestRequest) (providers.PullRequestResult, error)
	FindPullRequestByBranch(context.Context, providers.RepositoryRef, string, string) (providers.PullRequestResult, bool, error)
}

// Publisher dependencies are supplied by the authenticated host stage. The
// caller holds current publication authority across these bounded operations.
type Publisher struct {
	Queue *triggerqueue.Store
	Git   GitTransport
	PRs   PRProvider
}

func (t Target) validate() error {
	if err := t.Identity.ValidateChildLineage(); err != nil {
		return err
	}
	if t.Identity.Child == nil || t.Child.RunID != t.Identity.RunID || t.Child.Identity.Gaggle != t.Identity.Gaggle || t.Child.Identity.ParentRunID != t.Identity.Child.ParentRunID || t.Child.Identity.StageOccurrence != t.Identity.Child.StageOccurrence || t.Child.Identity.InvocationKey != t.Identity.Child.InvocationKey || t.Child.AcceptanceID != t.Identity.Child.AcceptanceID || t.Stage == "" || len(t.Stage) > 256 || t.Workspace == "" || t.Remote == "" || t.Base == "" || t.Head == "" || t.Base == t.Head {
		return errors.New("child publication target lacks exact admitted custody")
	}
	if !strings.HasSuffix(t.Head, "/children/"+t.Identity.RunID) {
		return errors.New("child publication requires its unique child branch")
	}
	return nil
}

// Push creates or reconciles one immutable branch snapshot per accepted child.
func (p Publisher) Push(ctx context.Context, t Target) (BranchReceipt, error) {
	var zero BranchReceipt
	if p.Queue == nil || p.Git == nil {
		return zero, errors.New("child publication unavailable")
	}
	if err := t.validate(); err != nil {
		return zero, err
	}
	ctx, cancel := context.WithTimeout(ctx, EffectTimeout)
	defer cancel()
	snapshot, commit, err := recovery.CaptureChildPublication(ctx, t.Workspace, t.Identity.RunID, t.Fork, t.Child.AcceptedAt)
	if err != nil {
		return zero, err
	}
	intent := BranchIntent{Version: 1, RunID: t.Identity.RunID, Lineage: *t.Identity.Child, Stage: t.Stage, Repository: t.Repository, Remote: t.Remote, Base: t.Base, Head: t.Head, Snapshot: snapshot, Commit: commit}
	data, err := json.Marshal(intent)
	if err != nil {
		return zero, err
	}
	retained, err := p.Queue.PrepareChildPublication(ctx, t.Child.Identity, "branch", data)
	if err != nil {
		return zero, err
	}
	observed, err := p.Git.Head(ctx, t.Workspace, t.Remote, t.Head)
	if err != nil {
		return zero, err
	}
	if retained.State == "prepared" {
		if observed != "" {
			return zero, errors.New("child publication branch already exists without owned effect admission")
		}

	} else if observed == commit {
		return p.confirmBranch(ctx, retained, BranchReceipt{Head: t.Head, SHA: commit})
	} else if observed != "" || retained.State == "confirmed" {
		return zero, errors.New("child publication branch differs from retained intent")
	}
	if err = p.Queue.BeginChildPublicationEffect(ctx, retained); err != nil {
		return zero, err
	}
	// The creation lease expects absence even on replay. No rebase, force-update,
	// alternate branch or fresh intent can hide a foreign reference race.
	if err = p.Git.Create(ctx, t.Workspace, t.Remote, t.Head, commit); err != nil {
		return zero, err
	}
	observed, err = p.Git.Head(ctx, t.Workspace, t.Remote, t.Head)
	if err != nil {
		return zero, err
	}
	if observed != commit {
		return zero, errors.New("child publication push outcome remains unconfirmed")
	}
	return p.confirmBranch(ctx, retained, BranchReceipt{Head: t.Head, SHA: commit})
}

func (p Publisher) confirmBranch(ctx context.Context, intent triggerqueue.ChildPublication, receipt BranchReceipt) (BranchReceipt, error) {
	data, err := json.Marshal(receipt)
	if err != nil {
		return BranchReceipt{}, err
	}
	if err = p.Queue.ConfirmChildPublication(ctx, intent, data); err != nil {
		return BranchReceipt{}, err
	}
	return receipt, nil
}

// OpenPR creates or reconciles one immutable PR intent per accepted child.
func (p Publisher) OpenPR(ctx context.Context, t Target, title, body string, draft bool) (providers.PullRequestResult, error) {
	var zero providers.PullRequestResult
	if p.Queue == nil || p.PRs == nil || p.Git == nil {
		return zero, errors.New("child PR publication unavailable")
	}
	if err := t.validate(); err != nil {
		return zero, err
	}
	if err := validatePRText(title, body); err != nil {
		return zero, err
	}
	ctx, cancel := context.WithTimeout(ctx, EffectTimeout)
	defer cancel()
	branch, source, err := p.confirmedPRBranch(ctx, t)
	if err != nil {
		return zero, err
	}
	request := providers.PullRequestRequest{Repository: t.Repository, Head: t.Head, Base: t.Base, Title: title, Body: body, Draft: draft, RunID: t.Identity.RunID}
	data, err := json.Marshal(PRIntent{Version: 1, BranchDigest: branch.Digest, Request: request})
	if err != nil {
		return zero, err
	}
	intent, err := p.Queue.PrepareChildPublication(ctx, t.Child.Identity, "pr", data)
	if err != nil {
		return zero, err
	}
	if intent.State == "confirmed" {
		var result providers.PullRequestResult
		err = json.Unmarshal(intent.Receipt, &result)
		return result, err
	}
	observed, err := p.Git.Head(ctx, t.Workspace, t.Remote, t.Head)
	if err != nil {
		return zero, err
	}
	if observed != source.Commit {
		return zero, errors.New("child PR branch differs from confirmed publication snapshot")
	}
	result, err := p.performPR(ctx, t, intent, request)
	if err != nil {
		return zero, err
	}
	if !validObservedPR(result) {
		return zero, fmt.Errorf("child provider returned invalid PR receipt")
	}
	data, err = json.Marshal(result)
	if err != nil {
		return zero, err
	}
	if err = p.Queue.ConfirmChildPublication(ctx, intent, data); err != nil {
		return zero, err
	}
	return result, nil
}

func (p Publisher) confirmedPRBranch(ctx context.Context, t Target) (triggerqueue.ChildPublication, BranchIntent, error) {
	var source BranchIntent
	branch, err := p.Queue.ChildPublication(ctx, t.Child.Identity, "branch")
	if err != nil {
		return branch, source, err
	}
	if branch.State != "confirmed" {
		return branch, source, errors.New("child branch publication is not confirmed")
	}
	if err = json.Unmarshal(branch.Intent, &source); err != nil {
		return branch, source, err
	}
	if source.Repository != t.Repository || source.Remote != t.Remote || source.Head != t.Head || source.Base != t.Base || source.RunID != t.Identity.RunID || source.Lineage != *t.Identity.Child {
		return branch, source, errors.New("child PR target differs from confirmed branch")
	}
	return branch, source, nil
}
func (p Publisher) performPR(ctx context.Context, t Target, intent triggerqueue.ChildPublication, request providers.PullRequestRequest) (providers.PullRequestResult, error) {
	var zero providers.PullRequestResult
	result, found, err := p.PRs.FindPullRequestByBranch(ctx, t.Repository, t.Head, t.Base)
	if err != nil {
		return zero, err
	}
	if intent.State == "effect_pending" {
		if !found {
			return zero, errors.New("child PR outcome remains uncertain; no duplicate creation is permitted")
		}
	} else {
		if found {
			return zero, errors.New("child PR already exists without owned effect admission")
		}
		if err = p.Queue.BeginChildPublicationEffect(ctx, intent); err != nil {
			return zero, err
		}
		result, err = p.PRs.OpenPullRequest(ctx, request)
		if err != nil {
			return zero, err
		}
	}
	return result, nil
}

func validatePRText(title, body string) error {
	if title == "" || len(title) > 256 || len(body) > 16<<10 || !utf8.ValidString(title) || !utf8.ValidString(body) || strings.ContainsAny(title, "\x00\r\n") || strings.ContainsRune(body, 0) {
		return errors.New("child PR text exceeds publication bounds")
	}
	return nil
}
