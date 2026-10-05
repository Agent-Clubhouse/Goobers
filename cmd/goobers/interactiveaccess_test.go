package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

type interactiveTestAuthenticator struct{ principal *httpapi.Principal }

func (a interactiveTestAuthenticator) Authenticate(*http.Request) (*httpapi.Principal, error) {
	return a.principal, nil
}

func TestInteractiveDaemonAssemblyPermissionRouteAndCredential(t *testing.T) {
	t.Setenv("HUMAN_API_TOKEN", "human-source-secret")
	policy := &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: "https://identity.example", Subject: "alice"}}}, Actions: []apiv1.InteractiveAction{"backlog.read", "backlog.edit"}, Credentials: apiv1.InteractiveCredentialBindings{Backlog: "human-backlog"}}
	definitions := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}, Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "acme/issues"}, InteractiveAccess: policy}}}}
	setup := &schedulerSetup{Config: &instance.Config{InteractiveCredentials: []instance.InteractiveCredential{{Name: "human-backlog", Provider: "github", Owner: "acme", Repository: "issues", Token: instance.TokenRef{Env: "HUMAN_API_TOKEN"}}}}, Definitions: definitions, SharedRegistry: journal.NewRegistryScrubber()}
	session := &upSession{}
	session.setup = setup
	if err := session.configureInteractiveAccess(); err != nil {
		t.Fatal(err)
	}
	p := httpapi.Principal{Subject: "alice", Issuer: "https://identity.example", Roles: []httpapi.Role{httpapi.RoleOperate}}
	opts := append([]httpapi.HandlerOption(nil), session.apiHandlerOpts...)
	opts = append(opts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &p}))
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	read := func() apicontract.InteractiveCapabilities {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/gaggles/web/interactive-capabilities", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body)
		}
		var result apicontract.InteractiveCapabilities
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := read(); !result.Operator || !result.PolicyConfigured {
		t.Fatalf("permission=%+v", result)
	}
	if err := setup.InteractiveAccess.WithCredential(context.Background(), p, "web", "backlog.edit", interactiveaccess.Target{Kind: "backlog"}, func(_ context.Context, credential interactiveaccess.Credential) error {
		if credential.Value != "human-source-secret" {
			t.Fatal("wrong source")
		}
		if strings.Contains(string(setup.SharedRegistry.Scrub([]byte(credential.Value))), credential.Value) {
			t.Fatal("daemon did not register human credential for redaction")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	revoked := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{*definitions.Gaggles[0].DeepCopy()}}
	revoked.Gaggles[0].Spec.InteractiveAccess = nil
	reloader := &configReloader{setup: setup}
	published := false
	if err := reloader.publishInteractiveDefinitions(revoked, func() error { published = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if result := read(); !published || result.PolicyConfigured || result.Operator {
		t.Fatalf("reload permission=%+v", result)
	}
}

func TestInteractiveRestartProductionAssemblyInstallsDedicatedBuilders(t *testing.T) {
	session := &upSession{}
	session.setup = &schedulerSetup{Config: &instance.Config{}, Definitions: &instance.ConfigSet{}, SharedRegistry: journal.NewRegistryScrubber(), RunnerRegistry: newDaemonRunnerRegistry()}
	if err := session.configureInteractiveAccess(); err != nil {
		t.Fatal(err)
	}
	if session.setup.InteractiveRestartExecution == nil || session.setup.InteractiveRestartRecovery == nil || session.setup.RunnerRegistry.resolveInteractiveGeneration == nil {
		t.Fatal("dedicated restart and recovery builders were not installed")
	}
}
