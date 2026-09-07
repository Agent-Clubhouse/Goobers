package providers

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// StickyCommentTargetKind selects the provider-native comment transport.
type StickyCommentTargetKind string

const (
	StickyCommentIssue       StickyCommentTargetKind = "issue"
	StickyCommentPullRequest StickyCommentTargetKind = "pull-request"
)

// StickyCommentTarget identifies the issue or pull request carrying a report.
type StickyCommentTarget struct {
	Kind StickyCommentTargetKind
	ID   string
}

// StickyCommentResult describes the comment adopted or created by an upsert.
// DuplicateIDs are pre-existing marker matches left untouched; the oldest
// marker is always the canonical comment updated on subsequent runs.
type StickyCommentResult struct {
	Comment      Comment
	Created      bool
	Adopted      bool
	DuplicateIDs []string
}

type workItemStickyComments interface {
	ListComments(context.Context, RepositoryRef, string) ([]Comment, error)
	CreateWorkItemComment(context.Context, RepositoryRef, string, string) (Comment, error)
	UpdateWorkItemComment(context.Context, RepositoryRef, string, string, string) error
}

type pullRequestStickyComments interface {
	ListPullRequestThreadComments(context.Context, RepositoryRef, string) ([]Comment, error)
	PostPullRequestThreadComment(context.Context, RepositoryRef, string, string) (Comment, error)
	UpdatePullRequestThreadComment(context.Context, RepositoryRef, string, string) error
}

// UpsertStickyComment updates the oldest exact-marker match or creates one.
// A failed non-idempotent create is followed by a read: if the provider
// committed the POST but its response was lost, the marker is adopted rather
// than posted again. Multiple existing markers are resolved deterministically
// to the oldest comment and surfaced in DuplicateIDs.
func UpsertStickyComment(
	ctx context.Context,
	provider Provider,
	repo RepositoryRef,
	target StickyCommentTarget,
	marker, body string,
) (StickyCommentResult, error) {
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return StickyCommentResult{}, fmt.Errorf("sticky comment marker is required")
	}
	if target.ID == "" {
		return StickyCommentResult{}, fmt.Errorf("sticky comment target id is required")
	}
	content := marker
	if strings.TrimSpace(body) != "" {
		content += "\n" + strings.TrimSpace(body)
	}

	list, create, update, err := stickyCommentOperations(provider, repo, target)
	if err != nil {
		return StickyCommentResult{}, err
	}
	comments, err := list(ctx)
	if err != nil {
		return StickyCommentResult{}, fmt.Errorf("list sticky comments: %w", err)
	}
	if match, duplicates, ok := selectStickyComment(comments, marker); ok {
		if err := update(ctx, match.ID, content); err != nil {
			return StickyCommentResult{}, fmt.Errorf("update sticky comment %s: %w", match.ID, err)
		}
		match.Body = content
		return StickyCommentResult{Comment: match, DuplicateIDs: duplicates}, nil
	}

	created, createErr := create(ctx, content)
	if createErr == nil {
		created.Body = content
		return StickyCommentResult{Comment: created, Created: true}, nil
	}

	comments, readErr := list(ctx)
	if readErr != nil {
		return StickyCommentResult{}, fmt.Errorf("create sticky comment: %w (adoption read failed: %v)", createErr, readErr)
	}
	match, duplicates, ok := selectStickyComment(comments, marker)
	if !ok {
		return StickyCommentResult{}, fmt.Errorf("create sticky comment: %w", createErr)
	}
	if err := update(ctx, match.ID, content); err != nil {
		return StickyCommentResult{}, fmt.Errorf("adopt sticky comment %s after create failure: %w", match.ID, err)
	}
	match.Body = content
	return StickyCommentResult{
		Comment: match, Adopted: true, DuplicateIDs: duplicates,
	}, nil
}

type stickyList func(context.Context) ([]Comment, error)
type stickyCreate func(context.Context, string) (Comment, error)
type stickyUpdate func(context.Context, string, string) error

func stickyCommentOperations(
	provider Provider,
	repo RepositoryRef,
	target StickyCommentTarget,
) (stickyList, stickyCreate, stickyUpdate, error) {
	switch target.Kind {
	case StickyCommentIssue:
		p, ok := provider.(workItemStickyComments)
		if !ok {
			return nil, nil, nil, fmt.Errorf("provider %q does not support sticky issue comments", provider.Kind())
		}
		return func(ctx context.Context) ([]Comment, error) {
				return p.ListComments(ctx, repo, target.ID)
			}, func(ctx context.Context, body string) (Comment, error) {
				return p.CreateWorkItemComment(ctx, repo, target.ID, body)
			}, func(ctx context.Context, commentID, body string) error {
				return p.UpdateWorkItemComment(ctx, repo, target.ID, commentID, body)
			}, nil
	case StickyCommentPullRequest:
		if provider.Kind() == ProviderADO {
			p, ok := provider.(pullRequestStickyComments)
			if !ok {
				return nil, nil, nil, fmt.Errorf("provider %q does not support sticky pull-request comments", provider.Kind())
			}
			return func(ctx context.Context) ([]Comment, error) {
					return p.ListPullRequestThreadComments(ctx, repo, target.ID)
				}, func(ctx context.Context, body string) (Comment, error) {
					return p.PostPullRequestThreadComment(ctx, repo, target.ID, body)
				}, func(ctx context.Context, commentID, body string) error {
					return p.UpdatePullRequestThreadComment(ctx, repo, commentID, body)
				}, nil
		}
		p, ok := provider.(workItemStickyComments)
		if !ok {
			return nil, nil, nil, fmt.Errorf("provider %q does not support sticky pull-request comments", provider.Kind())
		}
		return func(ctx context.Context) ([]Comment, error) {
				return p.ListComments(ctx, repo, target.ID)
			}, func(ctx context.Context, body string) (Comment, error) {
				return p.CreateWorkItemComment(ctx, repo, target.ID, body)
			}, func(ctx context.Context, commentID, body string) error {
				return p.UpdateWorkItemComment(ctx, repo, target.ID, commentID, body)
			}, nil
	default:
		return nil, nil, nil, fmt.Errorf("unknown sticky comment target kind %q", target.Kind)
	}
}

func selectStickyComment(comments []Comment, marker string) (Comment, []string, bool) {
	matches := make([]Comment, 0)
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			matches = append(matches, comment)
		}
	}
	if len(matches) == 0 {
		return Comment{}, nil, false
	}
	sort.SliceStable(matches, func(i, j int) bool {
		left, right := matches[i], matches[j]
		if left.CreatedAt != nil && right.CreatedAt != nil && !left.CreatedAt.Equal(*right.CreatedAt) {
			return left.CreatedAt.Before(*right.CreatedAt)
		}
		if left.CreatedAt != nil && right.CreatedAt == nil {
			return true
		}
		if left.CreatedAt == nil && right.CreatedAt != nil {
			return false
		}
		return left.ID < right.ID
	})
	duplicates := make([]string, 0, len(matches)-1)
	for _, comment := range matches[1:] {
		duplicates = append(duplicates, comment.ID)
	}
	return matches[0], duplicates, true
}
