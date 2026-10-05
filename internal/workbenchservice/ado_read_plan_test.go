package workbenchservice

import (
	"context"
	"net/http"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/workbench"
)

func TestADOPlanningRefreshUsesCurrentInteractiveAuthorityAndExactBacklog(t *testing.T) {
	queries, batches, calls := 0, 0, 0
	service, g, principal := fixture(t, func(req *http.Request) (*http.Response, error) {
		calls++
		_, token, ok := req.BasicAuth()
		if !ok || token != "human-read-canary" || !strings.HasPrefix(req.URL.Path, "/org/Work Project/_apis/wit/") {
			t.Fatalf("wrong interactive source or credential: %s", req.URL)
		}
		var body string
		switch {
		case strings.HasSuffix(req.URL.Path, "/wiql"):
			queries++
			if req.URL.Query().Get("$top") != "2" {
				t.Fatalf("workbench read lost its page bound: %s", req.URL)
			}
			body = `{"workItems":[{"id":1},{"id":2}]}`
		case strings.HasSuffix(req.URL.Path, "/workitemsbatch"):
			batches++
			body = `{"value":[{"id":1,"rev":9,"url":"https://dev.azure.com/org/_apis/wit/workItems/1","fields":{"System.TeamProject":"Work Project","System.Title":"Human context","System.State":"Active","System.WorkItemType":"Task"}},null]}`
		case strings.HasSuffix(req.URL.Path, "/states"):
			body = `{"value":[{"name":"Active","category":"InProgress"}]}`
		default:
			t.Fatalf("unexpected read: %s", req.URL)
		}
		return issueResponse(req, http.StatusOK, body), nil
	})
	g.Spec.Project = apiv1.RepoRef{Provider: "ado", Owner: "org", Project: "Code Project", Name: "code"}
	g.Spec.Backlog = apiv1.BacklogRef{Provider: "ado", Project: "Work Project"}
	credential := instance.InteractiveCredential{Name: "human", Provider: "ado", Owner: "org", Project: "Work Project", Token: instance.TokenRef{Env: "HUMAN_BACKLOG"}}
	permissions, err := interactiveaccess.New([]apiv1.Gaggle{g}, []instance.InteractiveCredential{credential}, interactiveaccess.Dependencies{Registrar: &secretRegistry{}})
	if err != nil {
		t.Fatal(err)
	}
	service.Permissions = permissions
	for range 2 {
		page, err := service.Page(context.Background(), principal, g.Name, "items", workbench.BacklogPageRequest{Limit: 2})
		if err != nil || len(page.Items) != 1 || page.Items[0].Ref.SourceID != "1" || page.Omitted != 1 || !page.Partial {
			t.Fatalf("source read: %+v %v", page, err)
		}
	}
	if queries != 2 || batches != 2 {
		t.Fatalf("interactive refresh replayed completed reads: %d queries, %d batches", queries, batches)
	}
	before := calls
	g.Spec.InteractiveAccess.Actions = nil
	if err := service.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Page(context.Background(), principal, g.Name, "items", workbench.BacklogPageRequest{Limit: 2}); err == nil {
		t.Fatal("warm cache bypassed current human policy")
	}
	if calls != before {
		t.Fatal("revoked authority contacted provider")
	}
}
