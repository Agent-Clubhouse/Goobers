package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/readservice"
)

func TestWorkbenchHostInstallsHumanPRSelectionWithoutImplicitSources(t *testing.T) {
	setup, pin := interactiveExecutionFixture(t)
	u := &upSession{}
	u.setup, u.l = setup, pin.layout
	u.configureWorkbenchReads()
	p := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	options := append(u.apiHandlerOpts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &p}))
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), options...)
	if err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/api/v1/gaggles/example/workbench/sources/undeclared/pull-requests/42", nil))
	if out.Code != http.StatusNotFound || !strings.Contains(out.Body.String(), "workbench_source_not_found") {
		t.Fatal("picker absent or undeclared source accepted", out.Code, out.Body.String())
	}
}
