package workbenchservice

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/workbench"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type secretRegistry struct {
	mu     sync.Mutex
	values []string
}

func (r *secretRegistry) Register(value []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, string(value))
}
func fixture(t *testing.T, transport roundTrip) (*Service, apiv1.Gaggle, httpapi.Principal) {
	t.Helper()
	t.Setenv("HUMAN_BACKLOG", "human-read-canary")
	t.Setenv("GITHUB_TOKEN", "automation-must-not-be-used")
	g := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "team"}, Spec: apiv1.GaggleSpec{
		Project: apiv1.RepoRef{Provider: "github", Owner: "acme", Name: "code"}, Backlog: apiv1.BacklogRef{Provider: "github", Project: "acme/issues"},
		Workbench:         &apiv1.GaggleWorkbench{SchemaVersion: "sources/v1", Sources: []apiv1.WorkbenchSource{{Name: "items", Kind: "backlog", Objectives: &apiv1.WorkbenchObjectiveSelector{IDs: []string{"987654"}}}}},
		InteractiveAccess: &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: "https://identity.example", Subject: "alice"}}}, Actions: []apiv1.InteractiveAction{"backlog.read", "session.message"}, Credentials: apiv1.InteractiveCredentialBindings{Backlog: "human"}},
	}}
	registry := &secretRegistry{}
	permissions, err := interactiveaccess.New([]apiv1.Gaggle{g}, []instance.InteractiveCredential{{Name: "human", Provider: "github", Owner: "acme", Repository: "issues", Token: instance.TokenRef{Env: "HUMAN_BACKLOG"}}}, interactiveaccess.Dependencies{Registrar: registry})
	if err != nil {
		t.Fatal(err)
	}
	factory := ProviderFactory{SchedulerDirectory: t.TempDir(), Client: &http.Client{Transport: transport}, Registrar: registry}
	return &Service{Permissions: permissions, Backlog: factory.Backlog}, g, httpapi.Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}
}

const issueJSON = `{"id":987654,"number":42,"title":"Source owned","body":"Current context","state":"open","html_url":"https://github.com/acme/issues/issues/42","updated_at":"2026-10-04T12:00:00Z","labels":[],"assignees":[]}`

func issueResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Request: r, Header: http.Header{"Content-Type": []string{"application/json"}, "Etag": []string{`"revision-1"`}}, Body: io.NopCloser(strings.NewReader(body))}
}
func TestAuthorizedBacklogReadUsesStableIdentityAndRevalidatesSharedCache(t *testing.T) {
	var calls atomic.Int32
	service, g, p := fixture(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/repos/acme/issues/issues/42/parent" {
			return issueResponse(r, 404, `{"message":"Not Found"}`), nil
		}
		if r.URL.Path == "/repos/acme/issues/issues/42/dependencies/blocked_by" {
			return issueResponse(r, 200, `[]`), nil
		}
		if r.Header.Get("Authorization") != "Bearer human-read-canary" || r.URL.Host != "api.github.com" || r.URL.Path != "/repos/acme/issues/issues/42" {
			t.Fatalf("wrong authorized target: %s", r.URL)
		}
		if calls.Add(1) == 1 {
			return issueResponse(r, 200, issueJSON), nil
		}
		if r.Header.Get("If-None-Match") != `"revision-1"` {
			t.Fatal("interactive refresh did not conditionally revalidate")
		}
		return issueResponse(r, 304, ""), nil
	})
	for range 2 {
		item, err := service.Get(context.Background(), p, g.Name, "items", workbench.BacklogItemRequest{ID: "42", ExpectedSourceID: "987654"})
		if err != nil {
			t.Fatal(err)
		}
		if item.Ref.SourceID != "987654" || item.Locator.ID != "42" || !item.Objective || item.RelationshipCoverage.Parents != "partial" {
			t.Fatal(item)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("read reused an unvalidated scheduler snapshot")
	}
}
func TestBacklogReadDeniedBeforeProviderAndSanitizesProviderFailure(t *testing.T) {
	var calls atomic.Int32
	service, g, p := fixture(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("sensitive-provider-body")
	})
	if _, err := service.Get(context.Background(), p, g.Name, "foreign", workbench.BacklogItemRequest{ID: "42"}); err == nil {
		t.Fatal("foreign binding accepted")
	}
	stranger := p
	stranger.Subject = "mallory"
	if _, err := service.Page(context.Background(), stranger, g.Name, "items", workbench.BacklogPageRequest{Limit: 10}); err == nil {
		t.Fatal("foreign human accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized operation reached provider")
	}
	_, err := service.Get(context.Background(), p, g.Name, "items", workbench.BacklogItemRequest{ID: "42"})
	if err == nil || strings.Contains(err.Error(), "sensitive-provider-body") {
		t.Fatal("provider details escaped", err)
	}
	g.Spec.InteractiveAccess = nil
	if err = service.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	if _, err = service.Get(context.Background(), p, g.Name, "items", workbench.BacklogItemRequest{ID: "42"}); err == nil {
		t.Fatal("missing policy used automation fallback")
	}
	if calls.Load() != before {
		t.Fatal("missing policy contacted provider")
	}
}
func TestSessionReadJoinsRevocationWithoutReenteringPolicyLock(t *testing.T) {
	entered := make(chan struct{})
	service, g, p := fixture(t, func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	lease, err := service.Permissions.BeginSessionExecution(context.Background(), p, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := service.ForSession(&g, lease)
	if err != nil {
		t.Fatal(err)
	}
	changed := g.DeepCopy()
	changed.Spec.Workbench.Sources[0].Name = "other"
	if _, err = service.ForSession(changed, lease); !errors.Is(err, interactiveaccess.ErrDenied) {
		t.Fatal("retained source retargeted", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := reader.Get(context.Background(), "items", workbench.BacklogItemRequest{ID: "42"})
		lease.Close()
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not start")
	}
	changed = g.DeepCopy()
	changed.Spec.InteractiveAccess.Actions = nil
	applied := make(chan error, 1)
	go func() { applied <- service.Permissions.Apply([]apiv1.Gaggle{*changed}, nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("revoked read succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session read deadlocked against policy reload")
	}
	select {
	case err := <-applied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("policy did not join read")
	}
	if _, err = reader.Get(context.Background(), "items", workbench.BacklogItemRequest{ID: "42"}); err == nil {
		t.Fatal("revoked reader remained usable")
	}
}

func TestSourceMetadataViewHasNoCredentialsOrProviderReads(t *testing.T) {
	service, g, p := fixture(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("metadata view contacted provider")
		return nil, nil
	})
	page, err := service.Sources(context.Background(), p, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].BindingID != "items" || page.Items[0].Repository != "issues" || len(page.Generation) != 64 {
		t.Fatal(page)
	}
	stranger := p
	stranger.Subject = "other"
	if _, err = service.Sources(context.Background(), stranger, g.Name); err == nil {
		t.Fatal("metadata leaked to nonmember")
	}
}
