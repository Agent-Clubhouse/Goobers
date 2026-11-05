package gate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/providers"
)

// ErrCommentUpdateUnsupported identifies a provider adapter that cannot edit
// comments. Duplicate cleanup must not fall back to another publication.
var ErrCommentUpdateUnsupported = errors.New("provider comment editing unsupported")

// DuplicateReportCleanupUnsupported records an already-published intent whose
// duplicate comments need operator attention because editing is unsupported.
type DuplicateReportCleanupUnsupported struct {
	Canonical    providers.Comment
	DuplicateIDs []string
}

func (e *DuplicateReportCleanupUnsupported) Error() string {
	return fmt.Sprintf("duplicate terminal report cleanup unsupported: keeping comment %s, duplicate comments %s", e.Canonical.ID, strings.Join(e.DuplicateIDs, ", "))
}

// reconcileReportDuplicates receives only authenticated-own streak comments.
// Tombstones keep the intent marker, but omit the streak marker so they cannot
// seed a failure count or be chosen as the next rolling report.
func reconcileReportDuplicates(ctx context.Context, poster Commenter, repository providers.RepositoryRef, comments []providers.Comment, intent string) (bool, error) {
	var matches []providers.Comment
	for _, c := range comments {
		if strings.Contains(c.Body, intent) {
			matches = append(matches, c)
		}
	}
	if len(matches) < 2 {
		return len(matches) == 1, nil
	}
	sortReportComments(matches)
	canonical := matches[0]
	duplicateIDs := make([]string, 0, len(matches)-1)
	for _, c := range matches[1:] {
		duplicateIDs = append(duplicateIDs, c.ID)
	}
	for _, duplicate := range matches[1:] {
		if duplicate.ID == "" || canonical.ID == "" || duplicate.ID == canonical.ID {
			return true, fmt.Errorf("duplicate terminal reports lack distinct comment identities")
		}
		// Providers normally return a permalink. Missing links are still useful to
		// an operator and are rendered as an explicit comment reference.
		reference := "comment " + canonical.ID
		if canonical.URL != "" {
			reference = "[comment " + canonical.ID + "](" + canonical.URL + ")"
		}
		body := "Duplicate of " + reference + "; superseded.\n\n" + intent + "\n<!-- goobers:terminal-duplicate -->"
		if err := poster.UpdateComment(ctx, repository, duplicate.ID, body); err != nil {
			if errors.Is(err, ErrCommentUpdateUnsupported) {
				return true, &DuplicateReportCleanupUnsupported{Canonical: canonical, DuplicateIDs: duplicateIDs}
			}
			return true, fmt.Errorf("tombstone duplicate terminal report %s: %w", duplicate.ID, err)
		}
	}
	return true, nil
}

func sortReportComments(comments []providers.Comment) {
	allDated := !slices.ContainsFunc(comments, func(c providers.Comment) bool { return c.CreatedAt == nil })
	sort.SliceStable(comments, func(i, j int) bool {
		a, b := comments[i], comments[j]
		if allDated && !a.CreatedAt.Equal(*b.CreatedAt) {
			return a.CreatedAt.Before(*b.CreatedAt)
		}
		// GitHub, Gitea and Boards expose numeric comment IDs; use numeric order
		// rather than lexicographic order when timestamps are absent or equal.
		ai, ae := strconv.ParseUint(a.ID, 10, 64)
		bi, be := strconv.ParseUint(b.ID, 10, 64)
		if ae == nil && be == nil {
			return ai < bi
		}
		if (ae == nil) != (be == nil) {
			return ae == nil
		}
		return a.ID < b.ID
	})
}

// A second read catches duplicates that became visible during publication.
// It is defense in depth, not an atomic provider-side idempotency guarantee.
func reconcilePublishedReport(ctx context.Context, poster Commenter, repository providers.RepositoryRef, itemID, marker, intent string) error {
	comments, err := poster.ListComments(ctx, repository, itemID)
	if err != nil {
		return fmt.Errorf("relist published terminal report: %w", err)
	}
	comments, err = ownMarkedComments(ctx, poster, repository, comments, marker)
	if err != nil {
		return err
	}
	_, err = reconcileReportDuplicates(ctx, poster, repository, comments, intent)
	return err
}
