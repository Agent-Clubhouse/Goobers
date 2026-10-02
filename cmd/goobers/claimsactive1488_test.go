package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
)

// TestClaimsActiveShowsHeldClaimsInCLIAndStandaloneAPI covers #1488: the CLI
// and the API route agree on what is actively claimed, and an expired lease is
// not reported as active.
func TestClaimsActiveShowsHeldClaimsInCLIAndStandaloneAPI(t *testing.T) {
	root := initDemo(t)
	seedClaims(t, root, time.Now())

	code, stdout, stderr := runArgs(t, "claims", "active", root)
	if code != 0 {
		t.Fatalf("active: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{"ITEM ID", "HOLDER", "AGE", "issue-9", "run-b", "implement", readservice.ActiveClaimHolderLocal} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout=%q, want %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "issue-8") {
		t.Fatalf("expired lease reported as active: %q", stdout)
	}

	code, stdout, stderr = runArgs(t, "claims", "active", "--json", root)
	if code != 0 {
		t.Fatalf("active --json: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var cli readservice.ActiveClaimList
	if err := json.Unmarshal([]byte(stdout), &cli); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout)
	}
	if len(cli.Claims) != 1 || cli.Claims[0].ItemID != "issue-9" || cli.Claims[0].RunID != "run-b" {
		t.Fatalf("CLI claims = %+v, want only issue-9", cli.Claims)
	}

	isolateStandaloneReadModelCache(t)
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	api, err := standaloneDashboardAPI(layout, config, log.New(io.Discard, "", 0), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := api.close(); err != nil {
			t.Error(err)
		}
	}()
	response := httptest.NewRecorder()
	api.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.ClaimsActivePath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", apicontract.ClaimsActivePath, response.Code, response.Body.String())
	}
	var served readservice.ActiveClaimList
	if err := json.Unmarshal(response.Body.Bytes(), &served); err != nil {
		t.Fatal(err)
	}
	if len(served.Claims) != 1 || served.Claims[0].ItemID != cli.Claims[0].ItemID ||
		served.Claims[0].Workflow != cli.Claims[0].Workflow || served.Claims[0].Holder != cli.Claims[0].Holder {
		t.Fatalf("API claims = %+v, CLI claims = %+v", served.Claims, cli.Claims)
	}
}
