package decomposition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/goobers/goobers/providers"
)

const (
	ChildBatchMarkerPrefix = "goobers-decomposition-child:"
	trackingSectionStart   = "<!-- goobers-decomposition-tracking:v1:start -->"
	trackingSectionEnd     = "<!-- goobers-decomposition-tracking:v1:end -->"
	resumeMarkerPrefix     = "<!-- goobers-decomposition-resume:"
)

// WorkItemDependencyProvider is the native declared-dependency surface used by
// the publisher.
type WorkItemDependencyProvider interface {
	ListWorkItemBlockers(context.Context, providers.RepositoryRef, string) ([]providers.WorkItem, error)
	AttachWorkItemBlocker(context.Context, providers.AttachWorkItemBlockerRequest) error
}

// PublisherProvider is the complete provider surface needed by publication.
type PublisherProvider interface {
	WorkItemMutationProvider
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
	ReleaseWorkItemClaim(context.Context, providers.ClaimWorkItemRequest) (providers.WorkItem, error)
}

// Publisher executes the recoverable prepare/publish protocol from design §5.
type Publisher struct {
	Provider PublisherProvider
	Leaser   TargetLeaser
	Repo     providers.RepositoryRef
	RunID    string
}

// PublishedBatch is the verified output of a completed publication.
type PublishedBatch struct {
	PlanDigest string
	Children   []providers.WorkItem
}

type verificationConflictError struct {
	reason string
}

func (e *verificationConflictError) Error() string {
	return e.reason
}

// Publish resumes or completes one decomposition batch.
func (p Publisher) Publish(ctx context.Context, plan Plan) (_ PublishedBatch, resultErr error) {
	if p.Provider == nil || p.Leaser == nil || p.Repo.Name == "" || p.RunID == "" {
		return PublishedBatch{}, fmt.Errorf("publisher provider, leaser, repository, and run id are required")
	}
	digest, err := PlanDigest(plan)
	if err != nil {
		return PublishedBatch{}, err
	}
	defer func() {
		if resultErr == nil || !isPublicationConflict(resultErr) {
			return
		}
		if err := p.parkParent(ctx, plan.Parent.ID); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("park parent after publication conflict: %w", err))
		}
	}()
	parent, err := p.Provider.GetWorkItem(ctx, p.Repo, plan.Parent.ID)
	if err != nil {
		return PublishedBatch{}, err
	}
	primitives := Primitives{Provider: p.Provider, Leaser: p.Leaser}

	if published, conflict, err := findBatchRecord(ctx, p.Provider, p.Repo, plan.Parent.ID, PublishedBatchMarkerPrefix, digest); err != nil {
		return PublishedBatch{}, err
	} else if conflict {
		return PublishedBatch{}, &MarkerConflictError{Key: digest, Reason: "parent has a conflicting published batch record"}
	} else if published != "" {
		ids := recordList(published, "children")
		children, err := p.readChildren(ctx, ids)
		if err != nil {
			return PublishedBatch{}, err
		}
		if err := p.verify(ctx, plan, digest, children, true); err != nil {
			return PublishedBatch{}, err
		}
		if parent.HasLabel("goobers/status:decomposing") {
			parent, err = p.Provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
				Repository: p.Repo, ID: parent.ID, ExpectedRevision: parent.Revision,
				RemoveLabels: []string{"goobers/status:decomposing"},
			})
			if err != nil {
				return PublishedBatch{}, err
			}
		}
		if err := p.releaseClaim(ctx, parent.ID); err != nil {
			return PublishedBatch{}, err
		}
		return PublishedBatch{PlanDigest: digest, Children: children}, nil
	}
	prepared := PreparedBatchRecord(plan, digest)
	preparedRecord, preparedConflict, err := findBatchRecord(ctx, p.Provider, p.Repo, plan.Parent.ID, PreparedBatchMarkerPrefix, digest)
	if err != nil {
		return PublishedBatch{}, err
	}
	if preparedRecord != "" && preparedRecord != prepared {
		preparedConflict = true
	}
	if preparedConflict {
		return PublishedBatch{}, &MarkerConflictError{Key: digest, Reason: "parent has a conflicting prepared batch record"}
	}
	resuming, resumeConflict := parentResumeState(parent.Body, parent.ID, digest)
	if resumeConflict {
		return PublishedBatch{}, &MarkerConflictError{Key: digest, Reason: "parent has a conflicting decomposition resume marker"}
	}
	if preparedRecord == "" && !resuming && parent.Revision != plan.Parent.ObservedRevision {
		return PublishedBatch{}, &providers.RevisionConflictError{
			ItemID: parent.ID, Expected: plan.Parent.ObservedRevision, Actual: parent.Revision,
		}
	}

	parent, err = p.ensureParentPrepared(ctx, parent, digest, resuming)
	if err != nil {
		return PublishedBatch{}, err
	}
	if _, err := primitives.AppendMarkerComment(ctx, p.Repo, MarkerCommentRequest{
		ItemID: parent.ID, ExpectedRevision: parent.Revision,
		IdempotencyKey: digest + "/record/prepared", Body: prepared,
	}); err != nil {
		return PublishedBatch{}, err
	}

	children := make([]providers.WorkItem, 0, len(plan.Children))
	for _, childPlan := range plan.Children {
		parent, err = p.Provider.GetWorkItem(ctx, p.Repo, parent.ID)
		if err != nil {
			return PublishedBatch{}, err
		}
		child, err := primitives.CreateChild(ctx, CreateChildRequest{
			ParentID: parent.ID, ExpectedParentRevision: parent.Revision,
			IdempotencyKey: digest + "/child/" + childPlan.Key,
			Item: providers.CreateWorkItemRequest{
				Repository: p.Repo,
				Title:      childPlan.Title,
				Body:       childIssueBody(parent.ID, digest, childPlan),
				Labels:     append(append([]string(nil), childPlan.Labels...), providers.LabelApproved),
			},
		})
		if err != nil {
			return PublishedBatch{}, err
		}
		children = append(children, child)
	}

	if _, ok := p.Provider.(WorkItemHierarchyProvider); ok {
		for i := range children {
			parent, err = p.Provider.GetWorkItem(ctx, p.Repo, parent.ID)
			if err != nil {
				return PublishedBatch{}, err
			}
			children[i], err = p.Provider.GetWorkItem(ctx, p.Repo, children[i].ID)
			if err != nil {
				return PublishedBatch{}, err
			}
			if err := primitives.AttachChild(ctx, p.Repo, providers.AttachWorkItemChildRequest{
				ParentID: parent.ID, ChildID: children[i].ID,
				ExpectedParentRevision: parent.Revision, ExpectedChildRevision: children[i].Revision,
			}); err != nil {
				return PublishedBatch{}, err
			}
		}
	}
	if err := p.attachDependencies(ctx, plan, children); err != nil {
		return PublishedBatch{}, err
	}

	parent, err = p.Provider.GetWorkItem(ctx, p.Repo, parent.ID)
	if err != nil {
		return PublishedBatch{}, err
	}
	body := trackingBody(parent.Body, plan, children)
	if parent.Body != body {
		if _, err := p.Provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: p.Repo, ID: parent.ID, ExpectedRevision: parent.Revision, Body: &body,
		}); err != nil {
			return PublishedBatch{}, err
		}
	}
	if err := p.verify(ctx, plan, digest, children, false); err != nil {
		return PublishedBatch{}, err
	}

	for i := range children {
		child, err := p.Provider.GetWorkItem(ctx, p.Repo, children[i].ID)
		if err != nil {
			return PublishedBatch{}, err
		}
		if !child.HasLabel(providers.LabelReady) {
			child, err = p.Provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
				Repository: p.Repo, ID: child.ID, ExpectedRevision: child.Revision,
				AddLabels: []string{providers.LabelReady},
			})
			if err != nil {
				return PublishedBatch{}, err
			}
		}
		children[i] = child
	}
	if err := p.verify(ctx, plan, digest, children, true); err != nil {
		return PublishedBatch{}, err
	}

	parent, err = p.Provider.GetWorkItem(ctx, p.Repo, parent.ID)
	if err != nil {
		return PublishedBatch{}, err
	}
	if _, err := primitives.AppendMarkerComment(ctx, p.Repo, MarkerCommentRequest{
		ItemID: parent.ID, ExpectedRevision: parent.Revision,
		IdempotencyKey: digest + "/comment/parent", Body: "Published this decomposition as a verified batch. Children become eligible together at the published-record barrier.",
	}); err != nil {
		return PublishedBatch{}, err
	}
	for i := range children {
		child, err := p.Provider.GetWorkItem(ctx, p.Repo, children[i].ID)
		if err != nil {
			return PublishedBatch{}, err
		}
		if _, err := primitives.AppendMarkerComment(ctx, p.Repo, MarkerCommentRequest{
			ItemID: child.ID, ExpectedRevision: child.Revision,
			IdempotencyKey: digest + "/comment/child/" + plan.Children[i].Key,
			Body:           fmt.Sprintf("This issue is child `%s` in decomposition batch `%s` for #%s.", plan.Children[i].Key, digest, parent.ID),
		}); err != nil {
			return PublishedBatch{}, err
		}
	}

	parent, err = p.Provider.GetWorkItem(ctx, p.Repo, parent.ID)
	if err != nil {
		return PublishedBatch{}, err
	}
	publishedBody := PublishedBatchRecord(parent.ID, digest, childIDs(children))
	if _, err := primitives.AppendMarkerComment(ctx, p.Repo, MarkerCommentRequest{
		ItemID: parent.ID, ExpectedRevision: parent.Revision,
		IdempotencyKey: digest + "/record/published", Body: publishedBody,
	}); err != nil {
		return PublishedBatch{}, err
	}
	parent, err = p.Provider.GetWorkItem(ctx, p.Repo, parent.ID)
	if err != nil {
		return PublishedBatch{}, err
	}
	if _, err := p.Provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: p.Repo, ID: parent.ID, ExpectedRevision: parent.Revision,
		RemoveLabels: []string{"goobers/status:decomposing"},
	}); err != nil {
		return PublishedBatch{}, err
	}
	if err := p.releaseClaim(ctx, parent.ID); err != nil {
		return PublishedBatch{}, err
	}
	return PublishedBatch{PlanDigest: digest, Children: children}, nil
}

func parentPrepared(parent providers.WorkItem) bool {
	return parent.HasLabel(providers.LabelTracking) &&
		parent.HasLabel("goobers/status:decomposing") &&
		!parent.HasLabel(providers.LabelReady) &&
		!parent.HasLabel(providers.LabelNeedsHuman)
}

func (p Publisher) ensureParentPrepared(ctx context.Context, parent providers.WorkItem, digest string, resuming bool) (providers.WorkItem, error) {
	if parentPrepared(parent) {
		return parent, nil
	}
	var err error
	if !resuming {
		body := strings.TrimSpace(parent.Body) + "\n\n" + parentResumeMarker(parent.ID, digest)
		parent, err = p.Provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: p.Repo, ID: parent.ID, ExpectedRevision: parent.Revision,
			Body:      &body,
			AddLabels: []string{"goobers/status:decomposing"},
		})
		if err != nil {
			return providers.WorkItem{}, err
		}
	} else if !parent.HasLabel("goobers/status:decomposing") {
		parent, err = p.Provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: p.Repo, ID: parent.ID, ExpectedRevision: parent.Revision,
			AddLabels: []string{"goobers/status:decomposing"},
		})
		if err != nil {
			return providers.WorkItem{}, err
		}
	}
	return p.Provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: p.Repo, ID: parent.ID, ExpectedRevision: parent.Revision,
		AddLabels:    []string{providers.LabelTracking},
		RemoveLabels: []string{providers.LabelReady, providers.LabelNeedsHuman},
	})
}

func (p Publisher) releaseClaim(ctx context.Context, parentID string) error {
	_, err := p.Provider.ReleaseWorkItemClaim(ctx, providers.ClaimWorkItemRequest{
		Repository: p.Repo,
		ID:         parentID,
		RunID:      p.RunID,
	})
	return err
}

func (p Publisher) parkParent(ctx context.Context, parentID string) error {
	parent, err := p.Provider.GetWorkItem(ctx, p.Repo, parentID)
	if err != nil {
		return err
	}
	if !parent.HasLabel(providers.LabelNeedsHuman) ||
		!parent.HasLabel("goobers/status:decomposing") ||
		parent.HasLabel(providers.LabelReady) {
		if _, err := p.Provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository:       p.Repo,
			ID:               parent.ID,
			ExpectedRevision: parent.Revision,
			AddLabels:        []string{providers.LabelNeedsHuman, "goobers/status:decomposing"},
			RemoveLabels:     []string{providers.LabelReady},
		}); err != nil {
			return err
		}
	}
	return p.releaseClaim(ctx, parent.ID)
}

func isPublicationConflict(err error) bool {
	var markerConflict *MarkerConflictError
	var revisionConflict *providers.RevisionConflictError
	var verificationConflict *verificationConflictError
	return errors.As(err, &markerConflict) ||
		errors.As(err, &revisionConflict) ||
		errors.As(err, &verificationConflict)
}

func (p Publisher) attachDependencies(ctx context.Context, plan Plan, children []providers.WorkItem) error {
	needsDependencies := false
	for _, child := range plan.Children {
		needsDependencies = needsDependencies || len(child.DependsOn) > 0
	}
	if !needsDependencies {
		return nil
	}
	dependencies, ok := p.Provider.(WorkItemDependencyProvider)
	if !ok {
		return fmt.Errorf("provider %q does not support declared work item dependencies", p.Repo.Provider)
	}
	ids := make(map[string]string, len(children))
	for i, child := range children {
		ids[plan.Children[i].Key] = child.ID
	}
	for i, childPlan := range plan.Children {
		for _, dependency := range childPlan.DependsOn {
			blockerID := dependency
			if childID, found := ids[dependency]; found {
				blockerID = childID
			}
			item, err := p.Provider.GetWorkItem(ctx, p.Repo, children[i].ID)
			if err != nil {
				return err
			}
			blocker, err := p.Provider.GetWorkItem(ctx, p.Repo, blockerID)
			if err != nil {
				return err
			}
			existing, err := dependencies.ListWorkItemBlockers(ctx, p.Repo, item.ID)
			if err != nil {
				return err
			}
			if slices.ContainsFunc(existing, func(candidate providers.WorkItem) bool { return candidate.ID == blockerID }) {
				continue
			}
			if err := dependencies.AttachWorkItemBlocker(ctx, providers.AttachWorkItemBlockerRequest{
				Repository: p.Repo, ItemID: item.ID, BlockerID: blockerID,
				ExpectedItemRevision: item.Revision, ExpectedBlockerRevision: blocker.Revision,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p Publisher) verify(ctx context.Context, plan Plan, digest string, children []providers.WorkItem, ready bool) error {
	if len(children) != len(plan.Children) {
		return &verificationConflictError{reason: fmt.Sprintf("batch has %d children, want %d", len(children), len(plan.Children))}
	}
	parent, err := p.Provider.GetWorkItem(ctx, p.Repo, plan.Parent.ID)
	if err != nil {
		return err
	}
	wantBody := trackingBody(parent.Body, plan, children)
	if parent.Body != wantBody {
		return &verificationConflictError{reason: fmt.Sprintf("parent %s tracking body does not match plan", parent.ID)}
	}
	for i, planned := range plan.Children {
		item, err := p.Provider.GetWorkItem(ctx, p.Repo, children[i].ID)
		if err != nil {
			return err
		}
		if item.Title != planned.Title || unmarkedActionBody(item.Body) != childIssueBody(parent.ID, digest, planned) {
			return &verificationConflictError{reason: fmt.Sprintf("child %s content conflicts with plan key %q", item.ID, planned.Key)}
		}
		wantLabels := append(append([]string(nil), planned.Labels...), providers.LabelApproved)
		if ready {
			wantLabels = append(wantLabels, providers.LabelReady)
		} else {
			item.Labels = slices.DeleteFunc(append([]string(nil), item.Labels...), func(label string) bool {
				return label == providers.LabelReady
			})
		}
		if !sameLabels(item.Labels, wantLabels) {
			return &verificationConflictError{reason: fmt.Sprintf("child %s labels %v do not match %v", item.ID, item.Labels, wantLabels)}
		}
	}
	if hierarchy, ok := p.Provider.(WorkItemHierarchyProvider); ok {
		actual, err := hierarchy.ListWorkItemChildren(ctx, p.Repo, parent.ID)
		if err != nil {
			return err
		}
		if !sameIDs(actual, children) {
			return &verificationConflictError{reason: fmt.Sprintf("parent %s child links do not match batch", parent.ID)}
		}
	}
	if dependencies, ok := p.Provider.(WorkItemDependencyProvider); ok {
		keyIDs := make(map[string]string, len(children))
		for i, child := range children {
			keyIDs[plan.Children[i].Key] = child.ID
		}
		for i, planned := range plan.Children {
			actual, err := dependencies.ListWorkItemBlockers(ctx, p.Repo, children[i].ID)
			if err != nil {
				return err
			}
			want := make([]string, 0, len(planned.DependsOn))
			for _, dependency := range planned.DependsOn {
				if id := keyIDs[dependency]; id != "" {
					want = append(want, id)
				} else {
					want = append(want, dependency)
				}
			}
			if !sameStringSet(itemIDs(actual), want) {
				return &verificationConflictError{reason: fmt.Sprintf("child %s dependencies do not match plan", children[i].ID)}
			}
		}
	}
	return nil
}

func (p Publisher) readChildren(ctx context.Context, ids []string) ([]providers.WorkItem, error) {
	children := make([]providers.WorkItem, 0, len(ids))
	for _, id := range ids {
		child, err := p.Provider.GetWorkItem(ctx, p.Repo, id)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, nil
}

// PlanDigest returns the canonical digest used by every batch marker.
func PlanDigest(plan Plan) (string, error) {
	data, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("marshal decomposition plan: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// PreparedBatchRecord is the append-only prepare marker.
func PreparedBatchRecord(plan Plan, digest string) string {
	keys := make([]string, 0, len(plan.Children))
	for _, child := range plan.Children {
		keys = append(keys, child.Key)
	}
	return fmt.Sprintf("%s v1 parent=%s digest=%s keys=%s source=%s", PreparedBatchMarkerPrefix, plan.Parent.ID, digest, strings.Join(keys, ","), plan.Selection.SourceRunID)
}

// PublishedBatchRecord is the single batch commit point.
func PublishedBatchRecord(parentID, digest string, childIDs []string) string {
	return fmt.Sprintf("%s v1 parent=%s digest=%s children=%s", PublishedBatchMarkerPrefix, parentID, digest, strings.Join(childIDs, ","))
}

// ChildBatchMarker identifies a child and its expected parent-side commit.
func ChildBatchMarker(parentID, digest, key string) string {
	return fmt.Sprintf("%s v1 parent=%s digest=%s key=%s", ChildBatchMarkerPrefix, parentID, digest, key)
}

// ChildBatchIdentity parses an exact child marker line.
func ChildBatchIdentity(body string) (parentID, digest, key string, marked bool, conflict bool) {
	var markers []string
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, ChildBatchMarkerPrefix) {
			markers = append(markers, line)
		}
	}
	if len(markers) == 0 {
		return "", "", "", false, false
	}
	if len(markers) != 1 {
		return "", "", "", true, true
	}
	fields := recordFields(markers[0])
	parentID, digest, key = fields["parent"], fields["digest"], fields["key"]
	if fields["_version"] != "v1" || parentID == "" || digest == "" || key == "" {
		return parentID, digest, key, true, true
	}
	return parentID, digest, key, true, false
}

// PublishedRecordIncludes verifies the exact parent-side commit for a child.
func PublishedRecordIncludes(comments []providers.Comment, parentID, digest, childID string) (bool, bool) {
	var marker string
	for _, comment := range comments {
		for _, line := range strings.Split(strings.ReplaceAll(comment.Body, "\r\n", "\n"), "\n") {
			if strings.HasPrefix(line, PublishedBatchMarkerPrefix) {
				fields, valid := parseBatchRecord(line, PublishedBatchMarkerPrefix)
				if marker != "" || !valid || fields["parent"] != parentID || fields["digest"] != digest {
					return false, true
				}
				marker = line
			}
		}
	}
	if marker == "" {
		return false, false
	}
	return slices.Contains(recordList(marker, "children"), childID), false
}

func findBatchRecord(ctx context.Context, provider WorkItemMutationProvider, repo providers.RepositoryRef, parentID, prefix, digest string) (string, bool, error) {
	comments, err := provider.ListComments(ctx, repo, parentID)
	if err != nil {
		return "", false, err
	}
	var exact string
	for _, comment := range comments {
		for _, line := range strings.Split(strings.ReplaceAll(comment.Body, "\r\n", "\n"), "\n") {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			fields, valid := parseBatchRecord(line, prefix)
			if !valid || fields["parent"] != parentID || fields["digest"] != digest {
				return "", true, nil
			}
			if exact != "" {
				return "", true, nil
			}
			exact = line
		}
	}
	return exact, false, nil
}

func childIssueBody(parentID, digest string, child ChildPlan) string {
	return strings.TrimSpace(child.Body) +
		"\n\n## Acceptance criteria\n\n" + strings.TrimSpace(child.AcceptanceCriteria) +
		"\n\n## Validation boundary\n\n" + strings.TrimSpace(child.ValidationBoundary) +
		"\n\nParent: #" + parentID + "\n\n" + ChildBatchMarker(parentID, digest, child.Key)
}

func unmarkedActionBody(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, actionMarkerPrefix) ||
			strings.HasPrefix(line, "<!-- goobers-action-digest:v1 ") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

func trackingBody(current string, plan Plan, children []providers.WorkItem) string {
	preserved := withoutResumeMarker(current)
	if start := strings.Index(current, trackingSectionStart); start >= 0 {
		if end := strings.Index(current[start:], trackingSectionEnd); end >= 0 {
			preserved = withoutResumeMarker(current[start+end+len(trackingSectionEnd):])
		}
	}
	var b strings.Builder
	b.WriteString(trackingSectionStart)
	b.WriteString("\n## Decomposition\n\n")
	b.WriteString(strings.TrimSpace(plan.Summary))
	b.WriteString("\n\n")
	for i, child := range children {
		fmt.Fprintf(&b, "- [ ] #%s — %s\n", child.ID, plan.Children[i].Title)
	}
	b.WriteString(trackingSectionEnd)
	if preserved != "" {
		b.WriteString("\n\n")
		b.WriteString(preserved)
	}
	return b.String()
}

func parentResumeMarker(parentID, digest string) string {
	return fmt.Sprintf("%s v1 parent=%s digest=%s -->", resumeMarkerPrefix, parentID, digest)
}

func parentResumeState(body, parentID, digest string) (bool, bool) {
	var marker string
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, resumeMarkerPrefix) {
			continue
		}
		if marker != "" || line != parentResumeMarker(parentID, digest) {
			return false, true
		}
		marker = line
	}
	return marker != "", false
}

func withoutResumeMarker(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	lines = slices.DeleteFunc(lines, func(line string) bool {
		return strings.HasPrefix(line, resumeMarkerPrefix)
	})
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func parseBatchRecord(record, prefix string) (map[string]string, bool) {
	parts := strings.Fields(record)
	var names []string
	switch prefix {
	case PreparedBatchMarkerPrefix:
		names = []string{"parent", "digest", "keys", "source"}
	case PublishedBatchMarkerPrefix:
		names = []string{"parent", "digest", "children"}
	default:
		return nil, false
	}
	if len(parts) != len(names)+2 || parts[0] != prefix || parts[1] != "v1" {
		return nil, false
	}
	fields := map[string]string{"_version": "v1"}
	for i, name := range names {
		key, value, ok := strings.Cut(parts[i+2], "=")
		if !ok || key != name || value == "" {
			return nil, false
		}
		fields[key] = value
	}
	listField := "children"
	if prefix == PreparedBatchMarkerPrefix {
		listField = "keys"
	}
	values := strings.Split(fields[listField], ",")
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return nil, false
		}
		if _, exists := seen[value]; exists {
			return nil, false
		}
		seen[value] = struct{}{}
	}
	return fields, true
}

func recordFields(record string) map[string]string {
	parts := strings.Fields(record)
	fields := map[string]string{}
	for _, part := range parts {
		if part == "v1" {
			fields["_version"] = part
		} else if key, value, ok := strings.Cut(part, "="); ok {
			fields[key] = value
		}
	}
	return fields
}

func recordList(record, field string) []string {
	value := recordFields(record)[field]
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func childIDs(children []providers.WorkItem) []string { return itemIDs(children) }

func itemIDs(items []providers.WorkItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func sameLabels(a, b []string) bool { return sameStringSet(a, b) }

func sameIDs(a, b []providers.WorkItem) bool { return sameStringSet(itemIDs(a), itemIDs(b)) }

func sameStringSet(a, b []string) bool {
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
