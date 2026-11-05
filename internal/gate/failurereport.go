package gate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goobers/goobers/providers"
)

type failureReportBodyKey struct{}
type failureReportBody struct {
	body    string
	persist func(string) error
}

// WithFailureReportBody freezes a terminal report's first rendering durably
// before publication. A replay uses that rendering even if live labels change.
func WithFailureReportBody(ctx context.Context, body string, persist func(string) error) context.Context {
	return context.WithValue(ctx, failureReportBodyKey{}, failureReportBody{body: body, persist: persist})
}

func freezeFailureReportBody(ctx context.Context, body string) (string, error) {
	frozen, ok := ctx.Value(failureReportBodyKey{}).(failureReportBody)
	if !ok {
		return body, nil
	}
	if frozen.body != "" {
		body = frozen.body
	}
	if err := frozen.persist(body); err != nil {
		return "", err
	}
	return body, nil
}

func failureIntentMarker(repository providers.RepositoryRef, itemID, kind, runID, stage string, count int, reset bool) string {
	// JSON framing prevents ambiguous identities when user-controlled names
	// contain separators. CanonicalKey includes provider, project and server.
	identity, _ := json.Marshal([]any{repository.CanonicalKey(), itemID, kind, runID, stage, count, reset})
	return fmt.Sprintf("<!-- goobers:terminal-intent %x -->", sha256.Sum256(identity))
}

// CommentAuthorMatcher resolves the authenticated author for a scoped provider.
// It must never trust display names on providers with non-unique names.
type CommentAuthorMatcher interface {
	OwnCommentMatcher(context.Context, providers.RepositoryRef) (func(providers.Comment) bool, error)
}

// OwnCommentMatcher returns a fail-closed matcher for the provider's own identity.
// ADO requires a GUID; GitHub/Gitea use their unique authenticated login.
func OwnCommentMatcher(ctx context.Context, poster any, repository providers.RepositoryRef) (func(providers.Comment) bool, error) {
	if matcher, ok := poster.(CommentAuthorMatcher); ok {
		return matcher.OwnCommentMatcher(ctx, repository)
	}
	if identity, ok := poster.(interface {
		AuthenticatedIdentity(context.Context) (providers.ADOIdentity, error)
	}); ok {
		self, err := identity.AuthenticatedIdentity(ctx)
		return func(c providers.Comment) bool { return self.ID != "" && strings.EqualFold(c.AuthorID, self.ID) }, err
	}
	if repository.Provider == providers.ProviderADO {
		return func(providers.Comment) bool { return false }, nil
	}
	if identity, ok := poster.(interface {
		AuthenticatedLogin(context.Context) (string, error)
	}); ok {
		self, err := identity.AuthenticatedLogin(ctx)
		return func(c providers.Comment) bool { return self != "" && strings.EqualFold(c.Author, self) }, err
	}
	return func(providers.Comment) bool { return false }, nil
}

func ownMarkedComments(ctx context.Context, poster Commenter, repository providers.RepositoryRef, comments []providers.Comment, marker string) ([]providers.Comment, error) {
	var marked []providers.Comment
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			marked = append(marked, c)
		}
	}
	if len(marked) == 0 {
		return nil, nil
	}
	own, err := OwnCommentMatcher(ctx, poster, repository)
	if err != nil {
		return nil, fmt.Errorf("resolve report author: %w", err)
	}
	var trusted []providers.Comment
	for _, c := range marked {
		if own(c) {
			trusted = append(trusted, c)
		}
	}
	return trusted, nil
}
