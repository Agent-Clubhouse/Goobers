package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

// This file is the one place that decides who a pull-request comment speaks
// for: a person, Goobers itself, or third-party automation. Every stage that
// acts on human feedback (pr-comment-watch routing a PR to remediation,
// rebase-pr's human-comment cause, update-behind-pr's label clear) classifies
// through commentClassifier, so they cannot disagree about who is human
// (#6494: rebase-pr compared logins while pr-comment-watch read markers, and
// under identityMode=shared it dropped the feedback pr-comment-watch had
// routed).

// Identity modes. dedicated: Goobers runs as its own bot identity, so an
// unmarked comment by that identity is still Goobers'. shared: Goobers runs as
// a person's own identity (on Azure DevOps, typically the operator's own az
// login), so only a Goobers marker tells its comments apart and an unmarked
// comment by that identity is the person's.
const (
	commentIdentityDedicated = "dedicated"
	commentIdentityShared    = "shared"
)

// defaultCommentIdentityMode is shared on Azure DevOps, whose recommended
// local authentication is the operator's own identity, and dedicated
// elsewhere, where a bot account has always been required.
func defaultCommentIdentityMode(kind providers.ProviderKind) string {
	if kind == providers.ProviderADO {
		return commentIdentityShared
	}
	return commentIdentityDedicated
}

// commentIdentitySettings are the identityMode and excludeAuthors stage
// inputs, parsed and defaulted. Every stage that classifies comments reads
// them through readCommentIdentitySettings, so one name means one thing.
type commentIdentitySettings struct {
	mode           string
	excludeAuthors map[string]bool
}

// readCommentIdentitySettings reads identityMode (default by provider) and
// excludeAuthors (default none) from the stage inputs.
func readCommentIdentitySettings(kind providers.ProviderKind) (commentIdentitySettings, error) {
	mode := strings.ToLower(strings.TrimSpace(providerInput("identityMode", defaultCommentIdentityMode(kind))))
	if mode != commentIdentityDedicated && mode != commentIdentityShared {
		return commentIdentitySettings{}, fmt.Errorf("invalid identityMode %q (want %s or %s)", mode, commentIdentityDedicated, commentIdentityShared)
	}
	return commentIdentitySettings{
		mode:           mode,
		excludeAuthors: toLowerSet(splitLabelList(providerInput("excludeAuthors", ""))),
	}, nil
}

// commentIdentity is the identity a stage's credential authenticates as. key
// is what comment authors are compared against: the login on GitHub and
// Gitea, the immutable identity id on Azure DevOps (byID), where display
// names are neither stable nor unique.
type commentIdentity struct {
	login string
	key   string
	byID  bool
}

// loginCommentIdentity is the GitHub and Gitea identity: the login is the key.
func loginCommentIdentity(login string) commentIdentity {
	return commentIdentity{login: login, key: login}
}

// adoCommentIdentity is the Azure DevOps identity, keyed by its immutable id.
func adoCommentIdentity(self providers.ADOIdentity) commentIdentity {
	login := self.UniqueName
	if login == "" {
		login = self.DisplayName
	}
	return commentIdentity{login: login, key: self.ID, byID: true}
}

// commentOrigin is who a comment speaks for.
type commentOrigin int

const (
	commentFromHuman commentOrigin = iota
	commentFromGoobers
	commentFromAutomation
)

// commentClassifier decides each comment's origin. The rule, in order:
//
//  1. A comment carrying one of Goobers' machine-state payloads outside a
//     quote (a sticky verdict, remediation state, a merge-demotion record...)
//     is bookkeeping: automation, neither a person's feedback nor an answer
//     to it. This holds with or without the attribution footer, which a
//     manual stage run or an incomplete run environment omits.
//  2. A comment carrying a Goobers marker on a line of its own, outside a
//     quote, is Goobers' own, whoever the author is: Goobers may post
//     through a second credential with another login. The marker is
//     unsigned, so a pasted copy also passes (#6178 item 3).
//  3. In dedicated mode, an unmarked comment whose author is the credential's
//     own identity is Goobers' own too.
//  4. A Bot-typed author (GitHub) or one named in excludeAuthors is third-party
//     automation and speaks for neither side.
//  5. Everything else is human — including, in shared mode, an unmarked
//     comment by the credential's own identity.
type commentClassifier struct {
	selfKey        string
	byID           bool
	dedicated      bool
	excludeAuthors map[string]bool
}

func newCommentClassifier(self commentIdentity, settings commentIdentitySettings) commentClassifier {
	return commentClassifier{
		selfKey:        self.key,
		byID:           self.byID,
		dedicated:      settings.mode == commentIdentityDedicated,
		excludeAuthors: settings.excludeAuthors,
	}
}

func (c commentClassifier) origin(comment providers.Comment) commentOrigin {
	author := comment.Author
	if c.byID {
		author = comment.AuthorID
	}
	switch {
	case goobersBookkeepingComment(comment.Body):
		return commentFromAutomation
	case goobersMarkedComment(comment.Body):
		return commentFromGoobers
	case c.dedicated && author != "" && strings.EqualFold(author, c.selfKey):
		return commentFromGoobers
	case strings.EqualFold(comment.AuthorType, "bot") || c.excludeAuthors[strings.ToLower(author)]:
		return commentFromAutomation
	}
	return commentFromHuman
}

// goobersBookkeepingPayloads are the machine-state payloads Goobers'
// bookkeeping comments carry: sticky verdicts and their finding history,
// remediation and merge-demotion state, sibling and escalation records, the
// issue-spec pin and the post-merge handoff.
var goobersBookkeepingPayloads = []string{
	"<!-- verdict-json: ",
	"<!-- finding-set-history: ",
	"<!-- remediation-state: ",
	"<!-- merge-demotion-state: ",
	"<!-- blocked-on-sibling: ",
	"<!-- implementation-escalation: ",
	"<!-- issue-spec-pin: ",
	"<!-- post-merge-remediation: ",
}

// goobersBookkeepingComment reports whether body carries one of Goobers'
// machine-state payloads outside a quote. Such a comment records state; it
// neither asks for nor answers anything, so it counts neither as a person's
// feedback (under shared identity an unmarked one would) nor as an
// acknowledgement of one (a sticky first posted after a reviewer's comment
// would otherwise mask it under the legacy unbound rule).
func goobersBookkeepingComment(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue
		}
		for _, payload := range goobersBookkeepingPayloads {
			if strings.Contains(line, payload) {
				return true
			}
		}
	}
	return false
}

// goobersMarkedComment reports whether body carries a marker only Goobers
// writes: a valid attribution footer (providers.ParseAttribution) or a
// review-thread response marker, each on a line of its own. Quoted lines are
// ignored first, so a person quoting a Goobers comment in a reply is still a
// person.
func goobersMarkedComment(body string) bool {
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		if strings.HasPrefix(trimmed, reviewThreadResponseMarkerPrefix) {
			return true
		}
		kept = append(kept, line)
	}
	unquoted := strings.Join(kept, "\n")
	if !strings.Contains(unquoted, "\n"+providers.AttributionMarkerPrefix) && !strings.HasPrefix(unquoted, providers.AttributionMarkerPrefix) {
		return false
	}
	_, ok, err := providers.ParseAttribution(unquoted)
	return ok && err == nil
}

// prCommentMark is a position in the watermark order: timestamp, then list
// position. Providers return comments oldest-first (Azure DevOps by publish
// time, thread id, then comment id), so position breaks timestamp ties and
// orders comments with no timestamp.
type prCommentMark struct {
	at  time.Time
	idx int
}

func (m prCommentMark) after(o prCommentMark) bool {
	return m.at.After(o.at) || (m.at.Equal(o.at) && m.idx > o.idx)
}

// prCommentAckMark is how far one Goobers comment acknowledges human feedback,
// and whether a feedback-ack frontier bounds it (#6918). A bound comment
// covers every human comment within the frontier's second, wherever it sits
// in the list; a "none" or unreadable frontier covers nothing. An unbound
// comment keeps the legacy rule and covers everything before itself.
func prCommentAckMark(c providers.Comment, at time.Time, idx, n int) (prCommentMark, bool) {
	frontier, bound := parseFeedbackAck(c.Body)
	switch {
	case !bound:
		return prCommentMark{at: at, idx: idx}, false
	case frontier.IsZero():
		return prCommentMark{idx: -1}, true
	default:
		return prCommentMark{at: frontier.Add(time.Second - time.Nanosecond), idx: n}, true
	}
}

// latestUnaddressedHumanComment reports the newest human comment and whether
// it is fresh: past every Goobers acknowledgement (a PR with none compares as
// zero time). classifier decides each comment's origin; automation comments
// take part in neither watermark. unbound reports a newest human comment that
// only an unbound (legacy, frontier-less) Goobers comment acknowledges, so the
// result can say the acknowledgement rests on no assessed snapshot.
//
// This is the rule pr-comment-watch routes a PR to remediation on, so it is
// also the rule a stage must consult before clearing the needs-remediation
// label that routing set.
func latestUnaddressedHumanComment(comments []providers.Comment, classifier commentClassifier) (human providers.Comment, fresh, unbound bool) {
	humanMark, ack, boundAck := prCommentMark{idx: -1}, prCommentMark{idx: -1}, prCommentMark{idx: -1}
	for i, c := range comments {
		mark := prCommentMark{idx: i}
		if c.CreatedAt != nil {
			mark.at = *c.CreatedAt
		}
		switch classifier.origin(c) {
		case commentFromGoobers:
			covers, bound := prCommentAckMark(c, mark.at, i, len(comments))
			if covers.after(ack) {
				ack = covers
			}
			if bound && covers.after(boundAck) {
				boundAck = covers
			}
		case commentFromHuman:
			if mark.after(humanMark) {
				human, humanMark = c, mark
			}
		}
	}
	if humanMark.idx == -1 {
		return providers.Comment{}, false, false
	}
	fresh = humanMark.after(ack)
	return human, fresh, !fresh && humanMark.after(boundAck)
}
