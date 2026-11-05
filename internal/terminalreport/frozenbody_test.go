package terminalreport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

type frozenBodyPoster struct {
	comments   []providers.Comment
	readErr    error
	postErr    error
	bodies     []string
	beforePost func(string)
}

func (p *frozenBodyPoster) GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error) {
	return providers.WorkItem{}, p.readErr
}
func (p *frozenBodyPoster) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return p.comments, nil
}
func (p *frozenBodyPoster) AuthenticatedIdentity(context.Context) (providers.ADOIdentity, error) {
	return providers.ADOIdentity{ID: "bot-guid"}, nil
}
func (p *frozenBodyPoster) UpdateComment(context.Context, providers.RepositoryRef, string, string) error {
	return errors.New("editing unsupported")
}
func (p *frozenBodyPoster) UpdateWorkItem(_ context.Context, r providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	if p.beforePost != nil {
		p.beforePost(r.Comment)
	}
	p.bodies = append(p.bodies, r.Comment)
	p.comments = append(p.comments, providers.Comment{Body: r.Comment, AuthorID: "bot-guid"})
	return providers.WorkItem{}, p.postErr
}

func TestQueueFreezesRenderedBodyBeforePublication(t *testing.T) {
	for _, lostAck := range []bool{false, true} {
		t.Run(map[bool]string{false: "write-did-not-land", true: "lost-ack"}[lostAck], func(t *testing.T) {
			now := time.Now()
			q := Queue{Dir: t.TempDir(), Now: func() time.Time { return now }}
			e := Effect{Kind: WorkFailure, Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Name: "app"}, ItemID: "42", RunID: "run-2", Stage: "ci-poll", Count: 2}
			p := &frozenBodyPoster{readErr: errors.New("lookup unavailable"), postErr: errors.New("connection lost")}
			p.beforePost = func(body string) {
				saved, err := readEntry(filepath.Join(q.Dir, effectKey(e)+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if saved.Body != body || !strings.Contains(body, "goobers:terminal-intent") {
					t.Fatal("body and marker not persisted before write")
				}
			}
			apply := func(ctx context.Context, e Effect) error { return e.Apply(ctx, p) }
			if err := q.Publish(context.Background(), e, apply); err == nil {
				t.Fatal("expected failed acknowledgement")
			}
			if !lostAck {
				p.comments = nil
			}
			p.readErr, p.postErr = nil, nil
			now = now.Add(2 * time.Minute)
			restarted := Queue{Dir: q.Dir, Now: q.Now}
			if err := restarted.Reconcile(context.Background(), 1, apply); err != nil {
				t.Fatal(err)
			}
			if lostAck && len(p.bodies) != 1 {
				t.Fatal("lost acknowledgement duplicated comment")
			}
			if !lostAck && (len(p.bodies) != 2 || p.bodies[0] != p.bodies[1]) {
				t.Fatal("replay changed the frozen instructions")
			}
			if _, err := os.Stat(filepath.Join(q.Dir, effectKey(e)+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("intent not acknowledged: %v", err)
			}
		})
	}
}
