package decomposition

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/providers"
)

type publisherFake struct {
	mu           sync.Mutex
	parentID     string
	items        map[string]providers.WorkItem
	comments     map[string][]providers.Comment
	children     []string
	blockers     map[string][]string
	nextID       int
	mutations    int
	failMutation int
}

var _ WorkItemDependencyProvider = (*providers.GitHubProvider)(nil)

func newPublisherFake() *publisherFake {
	return &publisherFake{
		parentID: "7",
		items: map[string]providers.WorkItem{
			"7": {ID: "7", Revision: "r1", Title: "Large change", Body: "Human context.", Labels: []string{providers.LabelApproved, providers.LabelReady, providers.LabelNeedsHuman}},
		},
		comments: map[string][]providers.Comment{},
		blockers: map[string][]string{},
		nextID:   8,
	}
}

func (f *publisherFake) GetWorkItem(_ context.Context, _ providers.RepositoryRef, id string) (providers.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[id]
	if !ok {
		return providers.WorkItem{}, fmt.Errorf("item %s not found", id)
	}
	return cloneWorkItem(item), nil
}

func (f *publisherFake) ListComments(_ context.Context, _ providers.RepositoryRef, id string) ([]providers.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]providers.Comment(nil), f.comments[id]...), nil
}

func (f *publisherFake) CreateWorkItem(_ context.Context, req providers.CreateWorkItemRequest) (providers.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprint(f.nextID)
	f.nextID++
	item := providers.WorkItem{ID: id, ExternalID: id, Revision: "r1", Title: req.Title, Body: req.Body, Labels: append([]string(nil), req.Labels...), State: "open"}
	f.items[id] = item
	return cloneWorkItem(item), f.afterMutation()
}

func (f *publisherFake) FindWorkItemsByMarker(_ context.Context, _ providers.RepositoryRef, marker string) ([]providers.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []providers.WorkItem
	for id, item := range f.items {
		if id != f.parentID && containsLine(item.Body, marker) {
			found = append(found, cloneWorkItem(item))
		}
	}
	return found, nil
}

func (f *publisherFake) CreateWorkItemComment(_ context.Context, _ providers.RepositoryRef, id, body string) (providers.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	comment := providers.Comment{ID: fmt.Sprintf("%s-%d", id, len(f.comments[id])+1), Body: body}
	f.comments[id] = append(f.comments[id], comment)
	return comment, f.afterMutation()
}

func (f *publisherFake) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[req.ID]
	if !ok {
		return providers.WorkItem{}, fmt.Errorf("item %s not found", req.ID)
	}
	if req.ExpectedRevision != "" && item.Revision != req.ExpectedRevision {
		return providers.WorkItem{}, &providers.RevisionConflictError{
			ItemID: req.ID, Expected: req.ExpectedRevision, Actual: item.Revision,
		}
	}
	if req.Title != nil {
		item.Title = *req.Title
	}
	if req.Body != nil {
		item.Body = *req.Body
	}
	for _, label := range req.RemoveLabels {
		item.Labels = slices.DeleteFunc(item.Labels, func(existing string) bool { return existing == label })
	}
	for _, label := range req.AddLabels {
		if !slices.Contains(item.Labels, label) {
			item.Labels = append(item.Labels, label)
		}
	}
	item.Revision = fmt.Sprintf("r%d", f.mutations+2)
	f.items[req.ID] = item
	return cloneWorkItem(item), f.afterMutation()
}

func (f *publisherFake) ListWorkItemChildren(context.Context, providers.RepositoryRef, string) ([]providers.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var children []providers.WorkItem
	for _, id := range f.children {
		children = append(children, cloneWorkItem(f.items[id]))
	}
	return children, nil
}

func (f *publisherFake) AttachWorkItemChild(_ context.Context, req providers.AttachWorkItemChildRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.children, req.ChildID) {
		f.children = append(f.children, req.ChildID)
	}
	return f.afterMutation()
}

func (f *publisherFake) ListWorkItemBlockers(_ context.Context, _ providers.RepositoryRef, id string) ([]providers.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var blockers []providers.WorkItem
	for _, blockerID := range f.blockers[id] {
		blockers = append(blockers, cloneWorkItem(f.items[blockerID]))
	}
	return blockers, nil
}

func (f *publisherFake) AttachWorkItemBlocker(_ context.Context, req providers.AttachWorkItemBlockerRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.blockers[req.ItemID], req.BlockerID) {
		f.blockers[req.ItemID] = append(f.blockers[req.ItemID], req.BlockerID)
	}
	return f.afterMutation()
}

func (f *publisherFake) afterMutation() error {
	f.mutations++
	if f.mutations == f.failMutation {
		return errors.New("injected provider failure after mutation")
	}
	return nil
}

func cloneWorkItem(item providers.WorkItem) providers.WorkItem {
	item.Labels = append([]string(nil), item.Labels...)
	return item
}

func testPublisherPlan() Plan {
	return Plan{
		SchemaVersion: PlanSchemaV1,
		Selection: PlanSelection{
			Mode: SelectionModeEscalation, SourceRunID: "source-1", IssueSnapshotDigest: "sha256:selection",
		},
		Parent:  ParentRef{Provider: "github", Repository: "acme/app", ID: "7", ObservedRevision: "r1"},
		Summary: "Split the oversized change into independently deliverable slices.",
		Children: []ChildPlan{
			{
				Key: "api", Title: "Build API", Body: "Implement the API behavior for this slice.",
				Labels: []string{"area:api", "type:feature"}, AcceptanceCriteria: "The API behavior is complete.", ValidationBoundary: "Run API package tests.",
			},
			{
				Key: "cli", Title: "Build CLI", Body: "Implement the CLI behavior for this slice.",
				Labels: []string{"area:cli", "type:feature"}, AcceptanceCriteria: "The CLI behavior is complete.", ValidationBoundary: "Run CLI package tests.", DependsOn: []string{"api"},
			},
		},
	}
}

func TestPublisherRecoversAfterEveryProviderMutation(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	const mutationBoundaries = 18
	for failAt := 1; failAt <= mutationBoundaries; failAt++ {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			fake := newPublisherFake()
			fake.failMutation = failAt
			publisher := Publisher{Provider: fake, Leaser: FileTargetLeaser{Directory: t.TempDir()}, Repo: repo}
			var batch PublishedBatch
			var err error
			for range 3 {
				batch, err = publisher.Publish(context.Background(), testPublisherPlan())
				if err == nil {
					break
				}
			}
			if err != nil {
				t.Fatalf("Publish after retry: %v", err)
			}
			if len(batch.Children) != 2 || len(fake.items) != 3 {
				t.Fatalf("children = %d, items = %d; want 2 and 3", len(batch.Children), len(fake.items))
			}
			if len(fake.children) != 2 || len(fake.blockers[batch.Children[1].ID]) != 1 {
				t.Fatalf("hierarchy = %v blockers = %v", fake.children, fake.blockers)
			}
			for id, comments := range fake.comments {
				keys := map[string]bool{}
				for _, comment := range comments {
					for _, line := range stringsLines(comment.Body) {
						if line != "" && len(line) > len(actionMarkerPrefix) && line[:len(actionMarkerPrefix)] == actionMarkerPrefix {
							if keys[line] {
								t.Fatalf("item %s has duplicate marker %q", id, line)
							}
							keys[line] = true
						}
					}
				}
			}
		})
	}
}

func TestChildPublicationBarrierMarkers(t *testing.T) {
	digest := "sha256:abc"
	body := "body\n\n" + ChildBatchMarker("7", digest, "api")
	parent, gotDigest, key, marked, conflict := ChildBatchIdentity(body)
	if parent != "7" || gotDigest != digest || key != "api" || !marked || conflict {
		t.Fatalf("identity = %q %q %q %v %v", parent, gotDigest, key, marked, conflict)
	}
	comments := []providers.Comment{{Body: PublishedBatchRecord("7", digest, []string{"8", "9"})}}
	if eligible, conflict := PublishedRecordIncludes(comments, "7", digest, "8"); !eligible || conflict {
		t.Fatalf("published = %v, conflict = %v", eligible, conflict)
	}
	if eligible, _ := PublishedRecordIncludes(comments, "7", digest, "10"); eligible {
		t.Fatal("unlisted child crossed publication barrier")
	}
}

func TestPublisherRejectsStaleParentBeforeMutation(t *testing.T) {
	fake := newPublisherFake()
	parent := fake.items[fake.parentID]
	parent.Revision = "r2"
	fake.items[fake.parentID] = parent
	publisher := Publisher{
		Provider: fake,
		Leaser:   FileTargetLeaser{Directory: t.TempDir()},
		Repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
	}
	_, err := publisher.Publish(context.Background(), testPublisherPlan())
	var conflict *providers.RevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want RevisionConflictError", err)
	}
	if fake.mutations != 0 {
		t.Fatalf("mutations = %d, want none", fake.mutations)
	}
}

func TestPublisherRejectsPublishedChildDrift(t *testing.T) {
	fake := newPublisherFake()
	publisher := Publisher{
		Provider: fake,
		Leaser:   FileTargetLeaser{Directory: t.TempDir()},
		Repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
	}
	batch, err := publisher.Publish(context.Background(), testPublisherPlan())
	if err != nil {
		t.Fatal(err)
	}
	child := fake.items[batch.Children[0].ID]
	child.Body += "\nhuman drift"
	fake.items[child.ID] = child
	if _, err := publisher.Publish(context.Background(), testPublisherPlan()); err == nil {
		t.Fatal("Publish after child drift succeeded, want conflict")
	}
}

func stringsLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		lines = append(lines, line)
	}
	return lines
}
