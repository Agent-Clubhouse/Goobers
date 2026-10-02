package providers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/mutationreceipt"
)

func (p *GitHubProvider) continuationReply(ctx context.Context, req PullRequestReviewThreadReply) (PullRequestInlineComment, error) {
	c := continuationClient(p)
	if req.CommentID <= 0 || strings.TrimSpace(req.Body) == "" {
		return PullRequestInlineComment{}, fmt.Errorf("comment id and reply body are required")
	}
	pull, err := c.pull(ctx, req.Repository, req.PullID)
	if err != nil {
		return PullRequestInlineComment{}, err
	}
	target := "pulls/" + req.PullID + "/comments/" + strconv.FormatInt(req.CommentID, 10) + "/replies"
	identity, err := c.identity(ctx, req.Repository, "review-thread-reply", target, struct{ Body, Head string }{StripAttribution(req.Body), pull.Head.SHA})
	if err != nil {
		return PullRequestInlineComment{}, err
	}
	endpoint, err := joinURL(c.baseURL, "repos", req.Repository.Owner, req.Repository.Name, target)
	if err != nil {
		return PullRequestInlineComment{}, err
	}
	var result PullRequestInlineComment
	err = c.session.Execute(ctx, identity, func(ctx context.Context, receipts []mutationreceipt.Receipt) (*mutationreceipt.Receipt, error) {
		comments, err := p.listInlinePullRequestComments(ctx, req.Repository, req.PullID)
		if err != nil {
			return nil, err
		}
		actor, err := p.AuthenticatedLogin(ctx)
		if err != nil {
			return nil, err
		}
		for _, receipt := range receipts {
			var found *githubInlineReviewComment
			for _, comment := range comments {
				if !containsExactLine(comment.Body, continuationMarker(receipt)) || !strings.EqualFold(comment.User.Login, actor) {
					continue
				}
				if comment.ID <= 0 || comment.InReplyTo != req.CommentID || continuationBody(comment.Body) != StripAttribution(req.Body) || found != nil {
					return nil, fmt.Errorf("%w: conflicting thread reply evidence", ErrMutationUnresolved)
				}
				copy := comment
				found = &copy
			}
			if found != nil {
				result = PullRequestInlineComment{ID: found.ID, Body: found.Body, InReplyTo: found.InReplyTo}
				return &receipt, nil
			}
		}
		return nil, nil
	}, func(ctx context.Context, receipt mutationreceipt.Receipt) error {
		body, err := stampContinuationBody(req.Body, p.attribution, "review-thread-reply", receipt)
		if err != nil {
			return err
		}
		var out githubInlineReviewComment
		if err := p.do(ctx, http.MethodPost, endpoint, map[string]string{"body": body}, &out); err != nil {
			return err
		}
		if out.ID <= 0 || out.InReplyTo != req.CommentID {
			return fmt.Errorf("%w: reply response lacks parent identity", ErrMutationUnresolved)
		}
		result = PullRequestInlineComment{ID: out.ID, Body: out.Body, InReplyTo: out.InReplyTo}
		p.recordExternalRef(ctx, ExternalRef{Provider: ProviderGitHub, Ref: issueRef(req.Repository, req.PullID), URL: out.HTMLURL, Operation: "review-thread-reply"})
		return nil
	})
	return result, err
}

const continuationThreadQuery = `query($threadId: ID!) {
 node(id: $threadId) { ... on PullRequestReviewThread {
  id isResolved
  repository { nameWithOwner }
  pullRequest { number headRefOid }
 } }
}`

type continuationThread struct {
	ID         string `json:"id"`
	Resolved   *bool  `json:"isResolved"`
	Repository struct {
		Name string `json:"nameWithOwner"`
	} `json:"repository"`
	Pull struct {
		Number int    `json:"number"`
		Head   string `json:"headRefOid"`
	} `json:"pullRequest"`
}

func (p *GitHubProvider) continuationThread(ctx context.Context, repo RepositoryRef, id string) (*continuationThread, error) {
	if err := requireOwnerRepo(repo); err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("review thread id is required")
	}
	var out struct {
		Node *continuationThread `json:"node"`
	}
	if err := p.graphql(mutationreceipt.FreshRead(ctx), continuationThreadQuery, map[string]interface{}{"threadId": id}, &out); err != nil {
		return nil, err
	}
	node := out.Node
	if node == nil || node.ID != id || node.Resolved == nil || node.Pull.Number <= 0 || node.Pull.Head == "" || !strings.EqualFold(node.Repository.Name, repo.Owner+"/"+repo.Name) {
		return nil, fmt.Errorf("%w: thread is not bound to repository/pull/head", ErrMutationUnresolved)
	}
	return node, nil
}

func (p *GitHubProvider) continuationResolve(ctx context.Context, repo RepositoryRef, id string) error {
	c := continuationClient(p)
	node, err := p.continuationThread(ctx, repo, id)
	if err != nil {
		return err
	}
	identity, err := c.identity(ctx, repo, "review-thread-resolve", "pulls/"+strconv.Itoa(node.Pull.Number)+"/threads/"+id, struct {
		Head     string
		Resolved bool
	}{node.Pull.Head, true})
	if err != nil {
		return err
	}
	return c.session.Execute(ctx, identity, func(ctx context.Context, receipts []mutationreceipt.Receipt) (*mutationreceipt.Receipt, error) {
		current, err := p.continuationThread(ctx, repo, id)
		if err != nil {
			return nil, err
		}
		if current.Pull != node.Pull || !*current.Resolved {
			return nil, nil
		}
		return &receipts[0], nil
	}, func(ctx context.Context, _ mutationreceipt.Receipt) error {
		if *node.Resolved {
			return nil
		}
		return p.resolvePullRequestReviewThread(ctx, repo, id)
	})
}
