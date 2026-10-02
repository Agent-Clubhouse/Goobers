package providers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// A work-item update is several provider calls — a field PATCH, label
// add/remove, a close, a comment — that can fail part-way through. Every
// effect except the comment is idempotent (a PATCH to the same value, a label
// add the forge dedupes, a label remove that tolerates 404), so the shared
// update paths below apply the comment last and, when the caller supplies an
// IdempotencyKey, stamp it with a hidden operation marker. A retry of the
// same request then replays only idempotent effects and adopts the comment
// an earlier attempt posted instead of appending a duplicate (#2657).

func updateRESTWorkItem(ctx context.Context, c restWorkItemMutator, kind ProviderKind, baseURL string, req UpdateWorkItemRequest) (WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	if req.ID == "" {
		return WorkItem{}, errIssueIDRequired
	}
	if req.Milestone != nil && *req.Milestone <= 0 {
		return WorkItem{}, fmt.Errorf("milestone number must be positive")
	}
	// A keyed update whose marked comment already exists applied every
	// effect on an earlier attempt (the comment is applied last). Replaying
	// nothing keeps the retry from undoing a later human edit and from
	// tripping ExpectedRevision on the revision that attempt produced.
	if applied, err := operationApplied(ctx, c, baseURL, req.Repository, req.ID, req.IdempotencyKey, req.Comment); err != nil {
		return WorkItem{}, err
	} else if applied {
		return c.GetWorkItem(ctx, req.Repository, req.ID)
	}
	before, err := c.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	if req.ExpectedRevision != "" {
		if err := checkWorkItemRevision(before, req.ExpectedRevision); err != nil {
			return WorkItem{}, err
		}
	}
	patch, patchFields, err := restWorkItemPatch(before, req)
	if err != nil {
		return WorkItem{}, err
	}

	update := newCompoundUpdate(c, ExternalRef{Provider: kind, Ref: issueRef(req.Repository, req.ID), Operation: updateOperation(req)})
	update.planIf(len(patch) > 0, "fields")
	update.planIf(labelsChanged(req), "labels")
	update.planIf(req.Comment != "", "comment")
	if len(patch) > 0 {
		if err := update.apply(ctx, "fields", patchFields, func() error {
			return patchRESTIssue(ctx, c, baseURL, req.Repository, req.ID, patch)
		}); err != nil {
			return WorkItem{}, err
		}
	}
	if labelsChanged(req) {
		after := applyLabelSet(before.Labels, req.AddLabels, req.RemoveLabels)
		labelFields := map[string]FieldDigest{"labels": {Before: digestLabels(before.Labels), After: digestLabels(after)}}
		if err := update.apply(ctx, "labels", labelFields, func() error {
			return c.applyLabelChanges(ctx, req.Repository, req.ID, req.AddLabels, req.RemoveLabels)
		}); err != nil {
			return WorkItem{}, err
		}
	}
	if req.Comment != "" {
		commentFields := map[string]FieldDigest{"comment": {After: digestString(req.Comment)}}
		if err := update.apply(ctx, "comment", commentFields, func() error {
			return postOperationComment(ctx, c, baseURL, req.Repository, req.ID, req.IdempotencyKey, req.Comment, func(body string) error {
				return c.postComment(ctx, req.Repository, req.ID, body)
			})
		}); err != nil {
			return WorkItem{}, err
		}
	}
	return update.finish(ctx, c, req.Repository, req.ID)
}

// restWorkItemPatch builds the single field PATCH an update sends, plus the
// before/after digest of each field it touches.
func restWorkItemPatch(before WorkItem, req UpdateWorkItemRequest) (map[string]interface{}, map[string]FieldDigest, error) {
	fields := map[string]FieldDigest{}
	patch := map[string]interface{}{}
	if req.Title != nil {
		patch["title"] = *req.Title
		fields["title"] = FieldDigest{Before: digestString(before.Title), After: digestString(*req.Title)}
	}
	if req.Body != nil {
		patch["body"] = *req.Body
		fields["body"] = FieldDigest{Before: digestString(before.Body), After: digestString(*req.Body)}
	}
	if req.Assignee != nil {
		assignees := []string{}
		if *req.Assignee != "" {
			assignees = append(assignees, *req.Assignee)
		}
		patch["assignees"] = assignees
		fields["assignee"] = FieldDigest{Before: digestString(before.Assignee), After: digestString(*req.Assignee)}
	}
	if req.Milestone != nil {
		milestoneBefore := ""
		if before.Parent != nil && before.Parent.Type == "milestone" {
			milestoneBefore = before.Parent.ID
		}
		patch["milestone"] = *req.Milestone
		fields["milestone"] = FieldDigest{Before: digestString(milestoneBefore), After: digestString(strconv.Itoa(*req.Milestone))}
	}
	if req.State != "" {
		state := strings.ToLower(req.State)
		if state != "open" && state != "closed" {
			return nil, nil, fmt.Errorf("unsupported state %q (want open or closed)", req.State)
		}
		patch["state"] = state
		fields["state"] = FieldDigest{Before: digestString(before.State), After: digestString(state)}
	}
	return patch, fields, nil
}

func patchRESTIssue(ctx context.Context, c restDoer, baseURL string, repo RepositoryRef, id string, patch map[string]interface{}) error {
	endpoint, err := joinURL(baseURL, "repos", repo.Owner, repo.Name, "issues", id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, endpoint, patch, nil)
}

// updateRESTWorkItemStatus mirrors a processing status onto an issue: swap
// the status label, close on done, then post the optional comment through
// post (which carries the provider's attribution action).
func updateRESTWorkItemStatus(ctx context.Context, c restWorkItemMutator, kind ProviderKind, baseURL string, req UpdateWorkItemStatusRequest, post func(body string) error) (WorkItem, error) {
	if err := requireOwnerRepo(req.Repository); err != nil {
		return WorkItem{}, err
	}
	if applied, err := operationApplied(ctx, c, baseURL, req.Repository, req.ID, req.IdempotencyKey, req.Comment); err != nil {
		return WorkItem{}, err
	} else if applied {
		return c.GetWorkItem(ctx, req.Repository, req.ID)
	}
	current, err := c.GetWorkItem(ctx, req.Repository, req.ID)
	if err != nil {
		return WorkItem{}, err
	}
	// Swap only the status label via the label sub-API (add new, remove any
	// stale status labels) rather than PATCHing the whole label set. A
	// read-modify-write of all labels would silently clobber a label a human or
	// the curator added between our GET above and the write — the status mirror
	// has no business overwriting unrelated labels (#140).
	newLabel := statusLabel(req.Status)
	var remove []string
	for _, l := range current.Labels {
		if strings.HasPrefix(l, statusLabelPrefix) && l != newLabel {
			remove = append(remove, l)
		}
	}
	done := req.Status == WorkItemStatusDone
	operation := "status"
	if done {
		operation = "close"
	}
	update := newCompoundUpdate(c, ExternalRef{Provider: kind, Ref: issueRef(req.Repository, req.ID), Operation: operation})
	update.planIf(true, "labels")
	update.planIf(done, "state")
	update.planIf(req.Comment != "", "comment")
	statusFields := map[string]FieldDigest{
		"status": {Before: digestString(string(statusFromLabels(current.Labels, current.State))), After: digestString(string(req.Status))},
	}
	if err := update.apply(ctx, "labels", statusFields, func() error {
		return c.applyLabelChanges(ctx, req.Repository, req.ID, []string{newLabel}, remove)
	}); err != nil {
		return WorkItem{}, err
	}
	if done {
		// state-only PATCH — labels are handled above, so closing never
		// round-trips (and races) the label set.
		if err := update.apply(ctx, "state", nil, func() error {
			return patchRESTIssue(ctx, c, baseURL, req.Repository, req.ID, map[string]interface{}{"state": "closed"})
		}); err != nil {
			return WorkItem{}, err
		}
	}
	if req.Comment != "" {
		if err := update.apply(ctx, "comment", nil, func() error {
			return postOperationComment(ctx, c, baseURL, req.Repository, req.ID, req.IdempotencyKey, req.Comment, post)
		}); err != nil {
			return WorkItem{}, err
		}
	}
	return update.finish(ctx, c, req.Repository, req.ID)
}

// PartialUpdateError reports a compound work-item update that failed after
// some of its effects committed (#2657). Completed and Pending name effects
// ("fields", "labels", "state", "comment") in application order; the effect
// that failed heads Pending and may or may not have reached the provider.
// Completed effects are already recorded as a mutation. Retrying the same
// request with the same IdempotencyKey is safe: completed effects are
// idempotent and the comment is adopted by its operation marker.
type PartialUpdateError struct {
	Ref       string
	Completed []string
	Pending   []string
	Err       error
}

func (e *PartialUpdateError) Error() string {
	return fmt.Sprintf("work item %s partially updated (completed: %s; pending: %s): %v",
		e.Ref, strings.Join(e.Completed, ","), strings.Join(e.Pending, ","), e.Err)
}

func (e *PartialUpdateError) Unwrap() error { return e.Err }

// compoundUpdate sequences the effects of one logical update and accumulates
// each committed effect's field digests, so a failure part-way through still
// journals what reached the provider rather than nothing.
type compoundUpdate struct {
	recorder  restMutationRecorder
	ref       ExternalRef
	pending   []string
	completed []string
}

func newCompoundUpdate(recorder restMutationRecorder, ref ExternalRef) *compoundUpdate {
	ref.Fields = map[string]FieldDigest{}
	return &compoundUpdate{recorder: recorder, ref: ref}
}

func (u *compoundUpdate) planIf(planned bool, effect string) {
	if planned {
		u.pending = append(u.pending, effect)
	}
}

// apply runs one planned effect. On failure it records the effects already
// committed and returns a *PartialUpdateError, or err unchanged when nothing
// had committed yet.
func (u *compoundUpdate) apply(ctx context.Context, effect string, fields map[string]FieldDigest, run func() error) error {
	if err := run(); err != nil {
		return u.fail(ctx, err)
	}
	for i, name := range u.pending {
		if name == effect {
			u.pending = append(u.pending[:i:i], u.pending[i+1:]...)
			break
		}
	}
	u.completed = append(u.completed, effect)
	for name, digest := range fields {
		u.ref.Fields[name] = digest
	}
	return nil
}

func (u *compoundUpdate) fail(ctx context.Context, err error) error {
	if len(u.completed) == 0 {
		return err
	}
	u.record(ctx)
	return &PartialUpdateError{
		Ref:       u.ref.Ref,
		Completed: append([]string{}, u.completed...),
		Pending:   append([]string{}, u.pending...),
		Err:       err,
	}
}

// finish reads the item back and records the whole update. Every effect has
// committed by now, so a failed read-back is still a partial result: the
// mutation is recorded and the read error returned with nothing pending.
func (u *compoundUpdate) finish(ctx context.Context, c restWorkItemMutator, repo RepositoryRef, id string) (WorkItem, error) {
	final, err := c.GetWorkItem(ctx, repo, id)
	if err != nil {
		return WorkItem{}, u.fail(ctx, err)
	}
	u.ref.URL = final.URL
	u.record(ctx)
	return final, nil
}

func (u *compoundUpdate) record(ctx context.Context) {
	if len(u.ref.Fields) > 0 {
		u.recorder.recordExternalRef(ctx, u.ref)
	}
}

// OperationCommentMarker is the hidden line identifying the comment of one
// logical update. The key is base64url-encoded so any caller-chosen key stays
// a single comment-safe line.
func OperationCommentMarker(key string) string {
	return "<!-- goobers:operation key=" + base64.RawURLEncoding.EncodeToString([]byte(key)) + " -->"
}

var operationMarkerPattern = regexp.MustCompile(`\n*<!-- goobers:operation key=[A-Za-z0-9_-]* -->\n?`)

// StripOperationMarker removes the operation marker a keyed update appends to
// its comment, then trims surrounding whitespace. Like StripAttribution it
// leaves other markers untouched; a reader matching the text Goobers meant to
// write strips both.
func StripOperationMarker(body string) string {
	return strings.TrimSpace(operationMarkerPattern.ReplaceAllString(body, "\n"))
}

// operationApplied reports whether a keyed update's marked comment already
// exists — meaning an earlier attempt applied the whole update, since the
// comment is its last effect. Unkeyed or comment-less updates carry no marker
// and are never treated as applied.
func operationApplied(ctx context.Context, c restPager, baseURL string, repo RepositoryRef, id, key, comment string) (bool, error) {
	if key == "" || comment == "" {
		return false, nil
	}
	return hasOperationComment(ctx, c, baseURL, repo, id, key)
}

func hasOperationComment(ctx context.Context, c restPager, baseURL string, repo RepositoryRef, id, key string) (bool, error) {
	comments, err := allIssueComments(ctx, c, baseURL, repo, id)
	if err != nil {
		return false, fmt.Errorf("look up operation comment: %w", err)
	}
	marker := OperationCommentMarker(key)
	for _, comment := range comments {
		if containsExactLine(comment.Body, marker) {
			return true, nil
		}
	}
	return false, nil
}

// postOperationComment appends body, stamped with the key's operation marker
// when key is set. A POST that failed after the forge committed it (a lost
// response) is adopted rather than reported by re-reading for the marker —
// the create-then-adopt shape decomposition's AppendMarkerComment uses.
func postOperationComment(ctx context.Context, c restPager, baseURL string, repo RepositoryRef, id, key, body string, post func(string) error) error {
	if key == "" {
		return post(body)
	}
	createErr := post(body + "\n\n" + OperationCommentMarker(key))
	if createErr == nil {
		return nil
	}
	found, lookupErr := hasOperationComment(ctx, c, baseURL, repo, id, key)
	if lookupErr != nil {
		return errors.Join(createErr, lookupErr)
	}
	if found {
		return nil
	}
	return createErr
}
