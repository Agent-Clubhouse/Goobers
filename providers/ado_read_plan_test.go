package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestADOReadPlanDeclarationIsTypedBoundedAndExact(t *testing.T) {
	endpoint := "https://dev.azure.com/org/project/_apis/wit/wiql?api-version=7.1"
	body := adoWIQLReadRequest{Query: "SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = @project"}
	ctx := context.WithValue(t.Context(), adoListReadKey{}, true)
	declared := declaredADOReadContext(ctx, http.MethodPost, endpoint, body)
	req, err := newJSONRequest(declared, http.MethodPost, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	if digest, ok := ADOReadPlanDigest(req); !ok || len(digest) != 64 {
		t.Fatal("typed list read not declared", digest, ok)
	}
	for _, test := range []struct {
		name string
		ctx  context.Context
		body any
		url  string
	}{
		{"not-list-context", t.Context(), body, endpoint},
		{"untyped-json", ctx, map[string]string{"query": body.Query}, endpoint},
		{"wrong-endpoint", ctx, body, strings.Replace(endpoint, "/wiql?", "/mutate?", 1)},
		{"oversized-query", ctx, adoWIQLReadRequest{Query: strings.Repeat("x", adoReadPlanMaxBytes)}, endpoint},
		{"unbounded-batch", ctx, adoWorkItemsBatchRequest{IDs: make([]int, 201), Expand: "Relations", ErrorPolicy: "Omit"}, strings.Replace(endpoint, "/wiql?", "/workitemsbatch?", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := newJSONRequest(declaredADOReadContext(test.ctx, http.MethodPost, test.url, test.body), http.MethodPost, test.url, test.body)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := ADOReadPlanDigest(request); ok {
				t.Fatal("undeclared request admitted")
			}
		})
	}
	changed := req.Clone(req.Context())
	changed.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{"query":"changed"}`)), nil }
	if _, ok := ADOReadPlanDigest(changed); ok {
		t.Fatal("changed body reused declaration")
	}
	changed = req.Clone(req.Context())
	changed.Method = http.MethodPatch
	if _, ok := ADOReadPlanDigest(changed); ok {
		t.Fatal("mutation reused declaration")
	}
}

func TestADOReadPlanResponseRejectsMalformedMembers(t *testing.T) {
	for _, test := range []struct {
		name, path, body string
		valid            bool
	}{
		{"empty-query", adoWIQLPath, `{"workItems":[]}`, true},
		{"query", adoWIQLPath, `{"workItems":[{"id":1}]}`, true},
		{"missing-query", adoWIQLPath, `{}`, false},
		{"null-query", adoWIQLPath, `{"workItems":null}`, false},
		{"null-reference", adoWIQLPath, `{"workItems":[null]}`, false},
		{"bad-reference", adoWIQLPath, `{"workItems":[{"id":"bad"}]}`, false},
		{"empty-batch", adoWorkItemsBatchPath, `{"value":[]}`, true},
		{"omitted-member", adoWorkItemsBatchPath, `{"value":[null]}`, true},
		{"batch", adoWorkItemsBatchPath, `{"value":[{"id":1,"fields":{}}]}`, true},
		{"bad-member", adoWorkItemsBatchPath, `{"value":["bad"]}`, false},
		{"bad-fields", adoWorkItemsBatchPath, `{"value":[{"id":1,"fields":[]}]}`, false},
		{"bad-relations", adoWorkItemsBatchPath, `{"value":[{"id":1,"fields":{},"relations":"bad"}]}`, false},
		{"missing-id", adoWorkItemsBatchPath, `{"value":[{"fields":{}}]}`, false},
		{"wrong-endpoint", "/mutation", `{"value":[]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://dev.azure.com/org/project"+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := ValidADOReadPlanResponse(req, []byte(test.body)); got != test.valid {
				t.Fatalf("response eligibility=%t, want %t", got, test.valid)
			}
		})
	}
}
