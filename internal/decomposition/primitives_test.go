package decomposition

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/goobers/goobers/providers"
)

type primitiveFake struct {
	mu           sync.Mutex
	parent       providers.WorkItem
	items        []providers.WorkItem
	comments     []providers.Comment
	children     []providers.WorkItem
	createCount  int
	commentCount int
	loseCreate   bool
	loseComment  bool
}

func (f *primitiveFake) GetWorkItem(_ context.Context, _ providers.RepositoryRef, id string) (providers.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == f.parent.ID {
		return f.parent, nil
	}
	for _, item := range f.items {
		if item.ID == id {
			return item, nil
		}
	}
	return providers.WorkItem{}, fmt.Errorf("item %s not found", id)
}

func (f *primitiveFake) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]providers.Comment(nil), f.comments...), nil
}

func (f *primitiveFake) CreateWorkItem(_ context.Context, req providers.CreateWorkItemRequest) (providers.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCount++
	item := providers.WorkItem{ID: fmt.Sprint(100 + f.createCount), Revision: "child-r1", Title: req.Title, Body: req.Body}
	f.items = append(f.items, item)
	if f.loseCreate {
		f.loseCreate = false
		return providers.WorkItem{}, errors.New("response lost")
	}
	return item, nil
}

func (f *primitiveFake) FindWorkItemsByMarker(_ context.Context, _ providers.RepositoryRef, marker string) ([]providers.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []providers.WorkItem
	for _, item := range f.items {
		if containsLine(item.Body, marker) {
			found = append(found, item)
		}
	}
	return found, nil
}

func (f *primitiveFake) CreateWorkItemComment(_ context.Context, _ providers.RepositoryRef, _ string, body string) (providers.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commentCount++
	comment := providers.Comment{ID: fmt.Sprint(f.commentCount), Body: body}
	f.comments = append(f.comments, comment)
	if f.loseComment {
		f.loseComment = false
		return providers.Comment{}, errors.New("response lost")
	}
	return comment, nil
}

func (f *primitiveFake) ListWorkItemChildren(context.Context, providers.RepositoryRef, string) ([]providers.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]providers.WorkItem(nil), f.children...), nil
}

func (f *primitiveFake) AttachWorkItemChild(_ context.Context, req providers.AttachWorkItemChildRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, item := range f.items {
		if item.ID == req.ChildID {
			f.children = append(f.children, item)
			return nil
		}
	}
	return errors.New("child not found")
}

func TestCreateChildLostResponseAndConcurrentRetryCreateOnce(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	fake := &primitiveFake{
		parent:     providers.WorkItem{ID: "7", Revision: "parent-r1"},
		loseCreate: true,
	}
	primitives := Primitives{
		Provider: fake,
		Leaser:   FileTargetLeaser{Directory: t.TempDir()},
	}
	req := CreateChildRequest{
		ParentID:               "7",
		ExpectedParentRevision: "parent-r1",
		IdempotencyKey:         "plan:abc/child:api",
		Item: providers.CreateWorkItemRequest{
			Repository: repo,
			Title:      "Build API",
			Body:       "Implement the API slice.",
			Labels:     []string{"area:api"},
		},
	}

	results := make(chan providers.WorkItem, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, err := primitives.CreateChild(context.Background(), req)
			results <- item
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("CreateChild: %v", err)
		}
	}
	var id string
	for item := range results {
		if id == "" {
			id = item.ID
		} else if item.ID != id {
			t.Fatalf("concurrent calls returned children %s and %s", id, item.ID)
		}
	}
	if fake.createCount != 1 || len(fake.items) != 1 {
		t.Fatalf("creates = %d, children = %d; want exactly one", fake.createCount, len(fake.items))
	}
}

func TestAppendMarkerCommentLostResponseAndConcurrentRetryCreateOnce(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	fake := &primitiveFake{
		parent:      providers.WorkItem{ID: "7", Revision: "parent-r1"},
		loseComment: true,
	}
	primitives := Primitives{
		Provider: fake,
		Leaser:   FileTargetLeaser{Directory: t.TempDir()},
	}
	req := MarkerCommentRequest{
		ItemID:           "7",
		ExpectedRevision: "parent-r1",
		IdempotencyKey:   "plan:abc/record:prepared",
		Body:             "Prepared decomposition batch abc.",
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := primitives.AppendMarkerComment(context.Background(), repo, req)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AppendMarkerComment: %v", err)
		}
	}
	if fake.commentCount != 1 || len(fake.comments) != 1 {
		t.Fatalf("posts = %d, comments = %d; want exactly one", fake.commentCount, len(fake.comments))
	}
}

func TestMarkerConflictRejectsReusedKey(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	fake := &primitiveFake{parent: providers.WorkItem{ID: "7", Revision: "parent-r1"}}
	primitives := Primitives{Provider: fake, Leaser: FileTargetLeaser{Directory: t.TempDir()}}
	first := MarkerCommentRequest{ItemID: "7", ExpectedRevision: "parent-r1", IdempotencyKey: "record", Body: "prepared"}
	if _, err := primitives.AppendMarkerComment(context.Background(), repo, first); err != nil {
		t.Fatal(err)
	}
	first.Body = "published"
	_, err := primitives.AppendMarkerComment(context.Background(), repo, first)
	var conflict *MarkerConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want MarkerConflictError", err)
	}
}

func TestCreateChildRejectsStaleParentRevisionBeforeWrite(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	fake := &primitiveFake{parent: providers.WorkItem{ID: "7", Revision: "parent-r2"}}
	primitives := Primitives{Provider: fake, Leaser: FileTargetLeaser{Directory: t.TempDir()}}
	_, err := primitives.CreateChild(context.Background(), CreateChildRequest{
		ParentID: "7", ExpectedParentRevision: "parent-r1", IdempotencyKey: "child",
		Item: providers.CreateWorkItemRequest{Repository: repo, Title: "Child", Body: "body"},
	})
	var conflict *providers.RevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want RevisionConflictError", err)
	}
	if fake.createCount != 0 {
		t.Fatalf("create count = %d, want 0", fake.createCount)
	}
}
