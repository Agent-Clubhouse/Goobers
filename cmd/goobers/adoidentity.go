package main

import (
	"context"
	"strings"

	"github.com/goobers/goobers/providers"
)

// adoIdentityReader is the Azure DevOps identity read the "is this me" checks
// on pull-request threads need (ADO-N5). Display names are not unique on ADO,
// so the stable key is the identity GUID (connectionData authenticatedUser.id).
type adoIdentityReader interface {
	AuthenticatedIdentity(ctx context.Context) (providers.ADOIdentity, error)
}

// adoCommentAuthoredBy reports whether comment was written by self. When the
// comment carries its author's identity GUID the comparison is by GUID only: a
// different identity that shares self's display name is not self, and self is
// recognized even when its display name has changed. A comment with no author
// GUID falls back to the display-name comparison the other providers use.
func adoCommentAuthoredBy(comment providers.Comment, self providers.ADOIdentity) bool {
	if id := strings.TrimSpace(comment.AuthorID); id != "" {
		return self.ID != "" && strings.EqualFold(id, self.ID)
	}
	return isTrustedMergeReviewAuthor(comment.Author, self.DisplayName)
}

// adoAttributeCommentsByID returns a copy of comments whose Author is
// rewritten so that the shared display-name trust checks (which compare
// Author against self.DisplayName) agree with adoCommentAuthoredBy: a comment
// self wrote carries self's current display name, and a comment another
// identity wrote under a colliding display name carries that name qualified by
// its GUID, so it can no longer pass as self.
func adoAttributeCommentsByID(comments []providers.Comment, self providers.ADOIdentity) []providers.Comment {
	out := make([]providers.Comment, len(comments))
	for i, comment := range comments {
		switch {
		case adoCommentAuthoredBy(comment, self):
			comment.Author = self.DisplayName
		case isTrustedMergeReviewAuthor(comment.Author, self.DisplayName):
			comment.Author = comment.Author + " (" + comment.AuthorID + ")"
		}
		out[i] = comment
	}
	return out
}
