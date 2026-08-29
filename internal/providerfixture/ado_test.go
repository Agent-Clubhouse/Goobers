package providerfixture

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefreshADONormalizesAndUsesProviderRequestShape(t *testing.T) {
	t.Parallel()
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:ado-pat"))
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("Authorization = %q, want Basic PAT auth", got)
		}
		if got := r.URL.Query().Get("api-version"); got != "7.1" {
			t.Errorf("api-version = %q, want 7.1", got)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", 100-round))
		switch r.URL.Path {
		case "/acme/Widgets/_apis/wit/wiql":
			if r.Method != http.MethodPost {
				t.Errorf("WIQL method = %s, want POST", r.Method)
			}
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode WIQL request: %v", err)
			}
			if !strings.Contains(request["query"], "ORDER BY [System.Id] ASC") {
				t.Errorf("WIQL query does not request stable order: %q", request["query"])
			}
			round++
			writeADOJSON(t, w, map[string]any{
				"asOf": fmt.Sprintf("2026-07-%02dT00:00:00Z", round),
				"workItems": []map[string]any{{
					"id": 7, "url": "https://dev.azure.com/acme/Widgets/_apis/wit/workItems/7",
				}},
			})
		case "/acme/Widgets/_apis/wit/workitems/7":
			if got := r.URL.Query().Get("$expand"); got != "Relations" {
				t.Errorf("$expand = %q, want Relations", got)
			}
			writeADOJSON(t, w, liveADOWorkItem(round))
		case "/acme/Widgets/_apis/wit/workitemtypes/Issue/states":
			writeADOJSON(t, w, map[string]any{"value": []map[string]string{
				{"name": "Active", "category": "InProgress"},
				{"name": "Closed", "category": "Completed"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := ADORefreshConfig{
		OrganizationURL: srv.URL + "/acme",
		Project:         "Widgets",
		WorkItem:        "7",
		Token:           "ado-pat",
	}
	first, err := RefreshADO(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RefreshADO(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDrift(first, second); err != nil {
		t.Fatalf("volatile ADO fields caused drift: %v", err)
	}
	raw, err := canonical(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"provider": "ado"`,
		`"owner": "fixture-org"`,
		`"name": "fixture-project"`,
		`"rev": 0`,
		`"id": "NORMALIZED"`,
		`"asOf": "2000-01-01T00:00:00Z"`,
		`"imageUrl": "NORMALIZED"`,
		`"System.CreatedDate": "2000-01-01T00:00:00Z"`,
		`https://dev.azure.com/fixture-org/fixture-project/_workitems/edit/7`,
		`"X-RateLimit-Remaining": "0"`,
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("normalized fixture does not contain %q:\n%s", want, raw)
		}
	}
	for _, secret := range []string{"ado-pat", wantBasicCredential("ado-pat")} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("fixture persisted its credential")
		}
	}
	if err := CheckContract(context.Background(), first); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshADORejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	valid := ADORefreshConfig{
		OrganizationURL: "https://dev.azure.com/acme",
		Project:         "project",
		WorkItem:        "7",
		Token:           "token",
	}
	cases := []struct {
		name string
		edit func(*ADORefreshConfig)
	}{
		{name: "organization URL", edit: func(cfg *ADORefreshConfig) { cfg.OrganizationURL = "https://dev.azure.com" }},
		{name: "project", edit: func(cfg *ADORefreshConfig) { cfg.Project = "" }},
		{name: "work item", edit: func(cfg *ADORefreshConfig) { cfg.WorkItem = "not-a-number" }},
		{name: "token", edit: func(cfg *ADORefreshConfig) { cfg.Token = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.edit(&cfg)
			if _, err := RefreshADO(context.Background(), cfg); err == nil {
				t.Fatal("RefreshADO() succeeded with invalid configuration")
			}
		})
	}
}

func TestADOProviderFixtureWorkflowIsDispatchOnly(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "provider-fixture-drift-ado.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(raw)
	for _, want := range []string{
		"workflow_dispatch:",
		"secrets.ADO_PAT",
		"vars.ADO_ORG_URL",
		"vars.ADO_PROJECT",
		"vars.ADO_PROVIDER_FIXTURE_WORK_ITEM",
		"-provider ado",
		"test/providers/testdata/ado_contract.json",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("ADO fixture workflow does not contain %q", want)
		}
	}
	if strings.Contains(workflow, "pull_request:") {
		t.Fatal("ADO provider fixture workflow must not run in pull-request CI")
	}
	for _, line := range strings.Split(workflow, "\n") {
		if strings.TrimSpace(line) == "schedule:" {
			t.Fatal("ADO provider fixture schedule must remain disabled")
		}
	}
}

func liveADOWorkItem(round int) map[string]any {
	return map[string]any{
		"id":  7,
		"rev": 100 + round,
		"url": "https://dev.azure.com/acme/Widgets/_workitems/edit/7",
		"fields": map[string]any{
			"System.WorkItemType": "Issue",
			"System.Title":        "Stable fixture work item",
			"System.Description":  "Stable fixture body.",
			"System.State":        "Active",
			"System.Tags":         "goobers:ready",
			"System.CreatedDate":  fmt.Sprintf("2026-07-%02dT01:02:03Z", round),
			"System.ChangedDate":  fmt.Sprintf("2026-07-%02dT04:05:06Z", round),
			"System.AssignedTo": map[string]any{
				"displayName": "Fixture User",
				"id":          fmt.Sprintf("identity-%d", round),
				"descriptor":  fmt.Sprintf("descriptor-%d", round),
				"imageUrl": fmt.Sprintf(
					"https://dev.azure.com/acme/_apis/GraphProfile/MemberAvatars/descriptor-%d",
					round,
				),
			},
		},
	}
}

func writeADOJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func wantBasicCredential(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("goobers:" + token))
}
