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
	called       chan<- struct{}
}

var _ WorkItemDependencyProvider = (*providers.GitHubProvider)(nil)

type publisherWithoutHierarchy struct {
	PublisherProvider
}

type gatedTargetLeaser struct {
	delegate TargetLeaser
	gate     <-chan struct{}
	started  chan<- struct{}
}

func (l gatedTargetLeaser) Acquire(ctx context.Context, repo providers.RepositoryRef, itemID string) (func() error, error) {
	select {
	case l.started <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-l.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return l.delegate.Acquire(ctx, repo, itemID)
}

func newPublisherFake() *publisherFake {
	return &publisherFake{
		parentID: "7",
		items: map[string]providers.WorkItem{
			"7": {ID: "7", Revision: "r1", Title: "Large change", Body: "Human context.", Labels: []string{providers.LabelApproved, providers.LabelReady, providers.LabelNeedsHuman, providers.LabelClaimed}},
		},
		comments: map[string][]providers.Comment{},
		blockers: map[string][]string{},
		nextID:   8,
	}
}

func (f *publisherFake) GetWorkItem(_ context.Context, _ providers.RepositoryRef, id string) (providers.WorkItem, error) {
	if f.called != nil {
		select {
		case f.called <- struct{}{}:
		default:
		}
	}
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
		f.storeUpdatedItem(req.ID, item)
		if err := f.afterMutation(); err != nil {
			return providers.WorkItem{}, err
		}
	}
	if len(req.AddLabels) > 0 {
		for _, label := range req.AddLabels {
			if !slices.Contains(item.Labels, label) {
				item.Labels = append(item.Labels, label)
			}
		}
		f.storeUpdatedItem(req.ID, item)
		if err := f.afterMutation(); err != nil {
			return providers.WorkItem{}, err
		}
	}
	for _, label := range req.RemoveLabels {
		item.Labels = slices.DeleteFunc(item.Labels, func(existing string) bool { return existing == label })
		f.storeUpdatedItem(req.ID, item)
		if err := f.afterMutation(); err != nil {
			return providers.WorkItem{}, err
		}
	}
	return cloneWorkItem(f.items[req.ID]), nil
}

func (f *publisherFake) storeUpdatedItem(id string, item providers.WorkItem) {
	item.Revision = fmt.Sprintf("r%d", f.mutations+2)
	f.items[id] = item
}

func (f *publisherFake) ReleaseWorkItemClaim(_ context.Context, req providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := f.items[req.ID]
	releaseMarker := "goobers-claim-release: run=" + req.RunID
	released := slices.ContainsFunc(f.comments[req.ID], func(comment providers.Comment) bool {
		return strings.Contains(comment.Body, releaseMarker)
	})
	if !released {
		comment := providers.Comment{ID: fmt.Sprintf("%s-%d", req.ID, len(f.comments[req.ID])+1), Body: releaseMarker}
		f.comments[req.ID] = append(f.comments[req.ID], comment)
		if err := f.afterMutation(); err != nil {
			return providers.WorkItem{}, err
		}
	}
	if item.HasLabel(providers.LabelClaimed) {
		item.Labels = slices.DeleteFunc(item.Labels, func(label string) bool { return label == providers.LabelClaimed })
		f.items[req.ID] = item
		if err := f.afterMutation(); err != nil {
			return providers.WorkItem{}, err
		}
	}
	return cloneWorkItem(item), nil
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
	probe := newPublisherFake()
	if _, err := (Publisher{
		Provider: probe, Leaser: FileTargetLeaser{Directory: t.TempDir()}, Repo: repo, RunID: "run-1",
	}).Publish(context.Background(), testPublisherPlan()); err != nil {
		t.Fatalf("baseline Publish: %v", err)
	}
	mutationBoundaries := probe.mutations
	for failAt := 1; failAt <= mutationBoundaries; failAt++ {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			fake := newPublisherFake()
			fake.failMutation = failAt
			publisher := Publisher{Provider: fake, Leaser: FileTargetLeaser{Directory: t.TempDir()}, Repo: repo, RunID: "run-1"}
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
				releases := 0
				for _, comment := range comments {
					if strings.Contains(comment.Body, "goobers-claim-release: run=run-1") {
						releases++
					}
					for _, line := range stringsLines(comment.Body) {
						if line != "" && len(line) > len(actionMarkerPrefix) && line[:len(actionMarkerPrefix)] == actionMarkerPrefix {
							if keys[line] {
								t.Fatalf("item %s has duplicate marker %q", id, line)
							}
							keys[line] = true
						}
					}
				}
				if releases > 1 {
					t.Fatalf("item %s has %d release comments, want at most one", id, releases)
				}
			}
			if fake.items[fake.parentID].HasLabel(providers.LabelClaimed) {
				t.Fatal("parent remains claimed after publication")
			}
		})
	}
}

func TestConcurrentPublishersSerializeBeforeProviderAccess(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	fake := newPublisherFake()
	providerCalled := make(chan struct{}, 1)
	fake.called = providerCalled
	gate := make(chan struct{})
	acquireStarted := make(chan struct{}, 2)
	leaser := gatedTargetLeaser{
		delegate: FileTargetLeaser{Directory: t.TempDir()},
		gate:     gate,
		started:  acquireStarted,
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := (Publisher{
				Provider: fake,
				Leaser:   leaser,
				Repo:     repo,
				RunID:    "run-1",
			}).Publish(context.Background(), testPublisherPlan())
			results <- err
		}()
	}

	providerAccessedBeforeLease := false
	for range 2 {
		select {
		case <-acquireStarted:
		case <-providerCalled:
			providerAccessedBeforeLease = true
			<-acquireStarted
		}
	}
	select {
	case <-providerCalled:
		providerAccessedBeforeLease = true
	default:
	}
	close(gate)
	for range 2 {
		if err := <-results; err != nil {
			t.Errorf("concurrent Publish: %v", err)
		}
	}
	if providerAccessedBeforeLease {
		t.Fatal("publisher accessed the provider before acquiring the batch target lease")
	}
	if len(fake.items) != 3 || len(fake.children) != 2 {
		t.Fatalf("items = %d, hierarchy = %v; want one two-child batch", len(fake.items), fake.children)
	}
}

func TestPublisherRetryPreservesHumanContextAroundTrackingSection(t *testing.T) {
	fake := newPublisherFake()
	fake.failMutation = 11
	publisher := Publisher{
		Provider: fake,
		Leaser:   FileTargetLeaser{Directory: t.TempDir()},
		Repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunID:    "run-1",
	}
	if _, err := publisher.Publish(context.Background(), testPublisherPlan()); err == nil {
		t.Fatal("Publish succeeded, want injected failure after tracking body update")
	}
	parent := fake.items[fake.parentID]
	if !strings.Contains(parent.Body, trackingSectionStart) {
		t.Fatalf("parent body after failure = %q, want tracking section", parent.Body)
	}

	before := "Human context added before the decomposition."
	after := "Human context added after the decomposition."
	parent.Body = before + "\n\n" + parent.Body + "\n\n" + after
	fake.items[parent.ID] = parent
	fake.failMutation = 0

	if _, err := publisher.Publish(context.Background(), testPublisherPlan()); err != nil {
		t.Fatalf("Publish after retry: %v", err)
	}
	body := fake.items[fake.parentID].Body
	start := strings.Index(body, trackingSectionStart)
	end := strings.Index(body, trackingSectionEnd)
	if beforeAt := strings.Index(body, before); beforeAt < 0 || beforeAt > start {
		t.Fatalf("parent body did not preserve context before tracking section: %q", body)
	}
	if afterAt := strings.Index(body, after); afterAt < end {
		t.Fatalf("parent body did not preserve context after tracking section: %q", body)
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
	comments = append(comments, providers.Comment{Body: PublishedBatchRecord("7", "sha256:other", []string{"10"})})
	if eligible, conflict := PublishedRecordIncludes(comments, "7", digest, "8"); eligible || !conflict {
		t.Fatalf("conflicting published record: eligible = %v, conflict = %v", eligible, conflict)
	}
	comments = []providers.Comment{{Body: PublishedBatchMarkerPrefix + " v2 parent=7 digest=" + digest + " children=8,9"}}
	if eligible, conflict := PublishedRecordIncludes(comments, "7", digest, "8"); eligible || !conflict {
		t.Fatalf("malformed published record: eligible = %v, conflict = %v", eligible, conflict)
	}
}

func TestChildBatchIdentityRejectsMalformedMarkers(t *testing.T) {
	for name, marker := range map[string]string{
		"extra field":      ChildBatchMarker("7", "sha256:abc", "api") + " extra=value",
		"reordered fields": ChildBatchMarkerPrefix + " v1 digest=sha256:abc parent=7 key=api",
		"duplicate field":  ChildBatchMarkerPrefix + " v1 parent=7 digest=sha256:abc parent=7",
		"missing field":    ChildBatchMarkerPrefix + " v1 parent=7 digest=sha256:abc",
		"wrong version":    ChildBatchMarkerPrefix + " v2 parent=7 digest=sha256:abc key=api",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, marked, conflict := ChildBatchIdentity(marker)
			if !marked || !conflict {
				t.Fatalf("marked = %v, conflict = %v; want malformed marker conflict", marked, conflict)
			}
		})
	}
}

func TestPublisherParksStaleParent(t *testing.T) {
	fake := newPublisherFake()
	parent := fake.items[fake.parentID]
	parent.Revision = "r2"
	fake.items[fake.parentID] = parent
	publisher := Publisher{
		Provider: fake,
		Leaser:   FileTargetLeaser{Directory: t.TempDir()},
		Repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunID:    "run-1",
	}
	_, err := publisher.Publish(context.Background(), testPublisherPlan())
	var conflict *providers.RevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want RevisionConflictError", err)
	}
	assertParentParked(t, fake)
}

func TestPublisherPersistsBatchIdentityBeforeQuarantine(t *testing.T) {
	plan := testPublisherPlan()
	digest, err := PlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	fake := newPublisherFake()
	fake.failMutation = 1
	publisher := Publisher{
		Provider: fake,
		Leaser:   FileTargetLeaser{Directory: t.TempDir()},
		Repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunID:    "run-1",
	}
	if _, err := publisher.Publish(context.Background(), plan); err == nil {
		t.Fatal("Publish succeeded, want injected failure")
	}
	parent := fake.items[fake.parentID]
	if resumed, conflict := parentResumeState(parent.Body, parent.ID, digest); !resumed || conflict {
		t.Fatalf("parent resume marker after first mutation = %v, %v; want durable batch identity", resumed, conflict)
	}
	if parentQuarantined(parent) {
		t.Fatalf("parent labels after first mutation = %v, quarantine must follow batch identity", parent.Labels)
	}

	otherPlan := plan
	otherPlan.Summary = "A different decomposition plan."
	fake.failMutation = 0
	_, err = publisher.Publish(context.Background(), otherPlan)
	var conflict *MarkerConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Publish different plan after retry boundary error = %v, want MarkerConflictError", err)
	}
}

func TestPublisherSkipsNativeHierarchyWhenUnsupported(t *testing.T) {
	fake := newPublisherFake()
	publisher := Publisher{
		Provider: publisherWithoutHierarchy{PublisherProvider: fake},
		Leaser:   FileTargetLeaser{Directory: t.TempDir()},
		Repo:     providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Name: "app"},
		RunID:    "run-1",
	}
	plan := testPublisherPlan()
	plan.Children[1].DependsOn = nil
	batch, err := publisher.Publish(context.Background(), plan)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(batch.Children) != 2 || len(fake.children) != 0 {
		t.Fatalf("published children = %d, native hierarchy = %v; want 2 children without native links", len(batch.Children), fake.children)
	}
	for _, child := range batch.Children {
		if !child.HasLabel(providers.LabelReady) {
			t.Fatalf("child %s labels = %v, want ready", child.ID, child.Labels)
		}
	}
}

func TestPublisherStillRequiresDeclaredDependencySupportWithoutHierarchy(t *testing.T) {
	fake := newPublisherFake()
	publisher := Publisher{
		Provider: publisherWithoutHierarchy{PublisherProvider: fake},
		Leaser:   FileTargetLeaser{Directory: t.TempDir()},
		Repo:     providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "acme", Name: "app"},
		RunID:    "run-1",
	}
	if _, err := publisher.Publish(context.Background(), testPublisherPlan()); err == nil ||
		!strings.Contains(err.Error(), "does not support declared work item dependencies") {
		t.Fatalf("Publish error = %v, want unsupported declared dependencies", err)
	}
}

func TestPublisherParksPreexistingTrackingWithoutResumeMarker(t *testing.T) {
	fake := newPublisherFake()
	parent := fake.items[fake.parentID]
	parent.Revision = "r2"
	parent.Labels = append(parent.Labels, providers.LabelTracking)
	fake.items[fake.parentID] = parent
	publisher := Publisher{
		Provider: fake,
		Leaser:   FileTargetLeaser{Directory: t.TempDir()},
		Repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunID:    "run-1",
	}
	_, err := publisher.Publish(context.Background(), testPublisherPlan())
	var conflict *providers.RevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want RevisionConflictError", err)
	}
	assertParentParked(t, fake)
}

func TestPublisherParksPreexistingDecomposingWithoutResumeMarker(t *testing.T) {
	fake := newPublisherFake()
	parent := fake.items[fake.parentID]
	parent.Revision = "r2"
	parent.Labels = append(parent.Labels, "goobers/status:decomposing")
	fake.items[fake.parentID] = parent
	publisher := Publisher{
		Provider: fake,
		Leaser:   FileTargetLeaser{Directory: t.TempDir()},
		Repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunID:    "run-1",
	}
	_, err := publisher.Publish(context.Background(), testPublisherPlan())
	var conflict *providers.RevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want RevisionConflictError", err)
	}
	assertParentParked(t, fake)
}

func TestPublisherRejectsMalformedSameDigestBatchRecord(t *testing.T) {
	plan := testPublisherPlan()
	digest, err := PlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []string{
		PreparedBatchMarkerPrefix + " v2 parent=7 digest=" + digest + " keys=api,cli source=source-1",
		PreparedBatchMarkerPrefix + " v1 parent=7 digest=" + digest + " keys=cli,api source=source-1",
		PublishedBatchMarkerPrefix + " v2 parent=7 digest=" + digest + " children=8,9",
		PublishedBatchMarkerPrefix + " v1 parent=7 digest=" + digest + " children=8,9 extra=value",
	} {
		t.Run(fmt.Sprint(strings.Fields(record)[0], "/", strings.Fields(record)[1], "/", len(strings.Fields(record))), func(t *testing.T) {
			fake := newPublisherFake()
			fake.comments[fake.parentID] = []providers.Comment{{Body: record}}
			publisher := Publisher{
				Provider: fake,
				Leaser:   FileTargetLeaser{Directory: t.TempDir()},
				Repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
				RunID:    "run-1",
			}
			_, err := publisher.Publish(context.Background(), plan)
			var conflict *MarkerConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("error = %v, want MarkerConflictError", err)
			}
			assertParentParked(t, fake)
		})
	}
}

func TestPublisherRejectsPublishedChildDrift(t *testing.T) {
	fake := newPublisherFake()
	publisher := Publisher{
		Provider: fake,
		Leaser:   FileTargetLeaser{Directory: t.TempDir()},
		Repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		RunID:    "run-1",
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
	assertParentParked(t, fake)
}

func assertParentParked(t *testing.T, fake *publisherFake) {
	t.Helper()
	parent := fake.items[fake.parentID]
	if !parent.HasLabel(providers.LabelNeedsHuman) ||
		!parent.HasLabel("goobers/status:decomposing") ||
		parent.HasLabel(providers.LabelReady) ||
		parent.HasLabel(providers.LabelClaimed) {
		t.Fatalf("parked parent labels = %v", parent.Labels)
	}
}

func stringsLines(value string) []string {
	return strings.Split(value, "\n")
}
