package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

const (
	// unansweredHumanFeedbackReasonCode prefixes merge-pr's refusal of a pull
	// request with open, unanswered human review feedback (#6134).
	// record-merge-refusal routes such a refusal to remediation.
	unansweredHumanFeedbackReasonCode = "unanswered-human-feedback"
	// humanFeedbackUnverifiableReasonCode prefixes the fail-closed refusal of
	// a pull request whose review feedback could not be read in full.
	humanFeedbackUnverifiableReasonCode = "human-feedback-unverifiable"
)

// mergeFeedbackGate is merge-pr's refusal to land a pull request over open,
// unanswered human review feedback (#6134). merge-review used to rely on the
// forge alone (GitHub's required conversation resolution, Azure DevOps'
// comment-resolution policy) and, on a repository without it, landed a PR
// whose reviewer had asked for a change minutes earlier.
//
// It is on by default; blockUnansweredFeedback=false opts out for teams that
// deliberately merge over comments. It fails closed: feedback that cannot be
// read in full, a provider with no review-thread read, or an invalid setting
// refuses as human-feedback-unverifiable instead of landing unverified.
type mergeFeedbackGate struct {
	disabled bool
	err      error
	settings commentIdentitySettings
	reader   mergeFeedbackReader
	// recheck reads the feedback again after a configured pre-merge check:
	// the check can take minutes, and a comment left meanwhile must not land
	// unread.
	recheck bool
}

// mergeFeedbackReader reads one pull request's feedback and the identity the
// comment classifier compares authors against.
type mergeFeedbackReader interface {
	feedback(ctx context.Context, pullID string) ([]providers.Comment, []feedbackThread, error)
	identity(ctx context.Context) (commentIdentity, error)
}

// newMergeFeedbackGate reads the gate's inputs (blockUnansweredFeedback,
// identityMode, excludeAuthors) and binds the provider's feedback read. A
// configuration error is kept and refuses at evaluation, inside the merge
// lock, like every other conjunct. preMergeCheck reports whether a pre-merge
// check runs between the first read and the land.
func newMergeFeedbackGate(repo providers.RepositoryRef, provider providers.Provider, verdictAuthor string, preMergeCheck bool) mergeFeedbackGate {
	raw := providerInput("blockUnansweredFeedback", "true")
	enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return mergeFeedbackGate{err: fmt.Errorf("invalid blockUnansweredFeedback %q (want true or false)", raw)}
	}
	if !enabled {
		return mergeFeedbackGate{disabled: true}
	}
	settings, err := readCommentIdentitySettings(repo.Provider)
	if err != nil {
		return mergeFeedbackGate{err: err}
	}
	reader, err := newMergeFeedbackReader(repo, provider, verdictAuthor)
	return mergeFeedbackGate{settings: settings, reader: reader, err: err, recheck: preMergeCheck}
}

// afterPreMergeCheck is the refusal set once the pre-merge check has run:
// its own refusal when it failed, else — when a check ran at all — a second
// read of the feedback immediately before the land. Unlike the head, which
// the land re-pins, feedback has no pin, so the earlier read alone would let
// a comment posted during a multi-minute build land unread.
func (g mergeFeedbackGate) afterPreMergeCheck(ctx context.Context, pullID, checkReason string) []string {
	if checkReason != "" {
		return []string{checkReason}
	}
	if !g.recheck {
		return nil
	}
	return g.refuse(ctx, pullID, nil)
}

// refuse appends the gate's refusal to reasons. It reads feedback only when
// every live conjunct before it already holds (reasons is empty), so a PR that
// is refused anyway costs no extra provider read.
func (g mergeFeedbackGate) refuse(ctx context.Context, pullID string, reasons []string) []string {
	if g.disabled || len(reasons) > 0 {
		return reasons
	}
	if reason := g.evaluate(ctx, pullID); reason != "" {
		return append(reasons, reason)
	}
	return reasons
}

func (g mergeFeedbackGate) evaluate(ctx context.Context, pullID string) string {
	if g.err != nil {
		return humanFeedbackUnverifiableReason(g.err)
	}
	self, err := g.reader.identity(ctx)
	if err != nil {
		return humanFeedbackUnverifiableReason(fmt.Errorf("resolve the authenticated identity: %w", err))
	}
	general, threads, err := g.reader.feedback(ctx, pullID)
	if err != nil {
		return humanFeedbackUnverifiableReason(err)
	}
	classifier := newCommentClassifier(self, g.settings)
	human, fresh, _ := latestUnaddressedHumanComment(openFeedbackComments(general, threads, classifier), classifier)
	if !fresh {
		return ""
	}
	return unansweredHumanFeedbackReason(human)
}

// unansweredHumanFeedbackReason renders the refusal for comment, the newest
// open human feedback no Goobers response acknowledges.
func unansweredHumanFeedbackReason(comment providers.Comment) string {
	who := comment.Author
	if who == "" {
		who = "a reviewer"
	}
	detail := ""
	if comment.CreatedAt != nil {
		detail += " at " + comment.CreatedAt.UTC().Format(time.RFC3339)
	}
	if comment.URL != "" {
		detail += " (" + comment.URL + ")"
	}
	return fmt.Sprintf("%s: %s left review feedback%s that no Goobers response has acknowledged — routing to remediation instead of landing over it %s",
		unansweredHumanFeedbackReasonCode, who, detail, feedbackCommentToken(comment))
}

func humanFeedbackUnverifiableReason(err error) string {
	return fmt.Sprintf("%s: the pull request's review feedback could not be read in full (%v) — refusing to land over feedback it cannot see",
		humanFeedbackUnverifiableReasonCode, err)
}

// adoReviewFeedbackProvider is the Azure DevOps feedback surface: every live
// thread with its status, and the credential's identity by immutable id.
type adoReviewFeedbackProvider interface {
	ListPullRequestFeedbackThreads(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]providers.PullRequestFeedbackThread, error)
	AuthenticatedIdentity(ctx context.Context) (providers.ADOIdentity, error)
}

func newMergeFeedbackReader(repo providers.RepositoryRef, provider providers.Provider, verdictAuthor string) (mergeFeedbackReader, error) {
	if ado, ok := provider.(adoReviewFeedbackProvider); ok {
		return adoMergeFeedbackReader{provider: ado, repo: repo}, nil
	}
	if forge, ok := provider.(forgeReviewFeedbackProvider); ok {
		return forgeMergeFeedbackReader{provider: forge, repo: repo, login: strings.TrimSpace(verdictAuthor)}, nil
	}
	return nil, fmt.Errorf("repository provider %q cannot list pull-request review threads", repo.Provider)
}

type forgeMergeFeedbackReader struct {
	provider forgeReviewFeedbackProvider
	repo     providers.RepositoryRef
	// login is the identity Goobers posts its comments as (apply-verdict's
	// verdictAuthor); empty falls back to the credential's own login.
	login string
}

func (r forgeMergeFeedbackReader) feedback(ctx context.Context, pullID string) ([]providers.Comment, []feedbackThread, error) {
	return readForgeFeedback(ctx, r.provider, r.repo, pullID)
}

func (r forgeMergeFeedbackReader) identity(ctx context.Context) (commentIdentity, error) {
	if r.login != "" {
		return loginCommentIdentity(r.login), nil
	}
	login, err := r.provider.AuthenticatedLogin(ctx)
	if err != nil {
		return commentIdentity{}, err
	}
	return loginCommentIdentity(login), nil
}

type adoMergeFeedbackReader struct {
	provider adoReviewFeedbackProvider
	repo     providers.RepositoryRef
}

func (r adoMergeFeedbackReader) feedback(ctx context.Context, pullID string) ([]providers.Comment, []feedbackThread, error) {
	threads, err := r.provider.ListPullRequestFeedbackThreads(ctx, r.repo, pullID)
	if err != nil {
		return nil, nil, fmt.Errorf("list threads on PR #%s: %w", pullID, err)
	}
	return nil, adoFeedbackThreads(threads), nil
}

func (r adoMergeFeedbackReader) identity(ctx context.Context) (commentIdentity, error) {
	self, err := r.provider.AuthenticatedIdentity(ctx)
	if err != nil {
		return commentIdentity{}, err
	}
	return adoCommentIdentity(self), nil
}
