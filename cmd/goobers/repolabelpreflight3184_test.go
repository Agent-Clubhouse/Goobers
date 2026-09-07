package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReservedLabelPreflightIncludesImplicitLifecycleLabels(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "backlog"}}}
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "team"}, Spec: apiv1.GaggleSpec{
			Project: apiv1.RepoRef{Provider: "github", Owner: "acme", Name: "backlog"},
		}}},
		Workflows: []apiv1.Workflow{{ObjectMeta: metav1.ObjectMeta{Name: "curate"}, Spec: apiv1.WorkflowSpec{
			Gaggle: "team", Tasks: []apiv1.Task{
				{Name: "health", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-health"}}},
				{Name: "resweep", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-resweep"}}},
				{Name: "close", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "issue-close-out"}}},
				{Name: "park", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "issue-close-out"}}, Inputs: map[string]string{"status": "needs-remediation"}},
				{Name: "claim", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim"}}},
			},
		}}},
	}
	demands := gatherRepoRealityDemand(".", "config", cfg, set)
	demand := demands[0]
	if demand == nil {
		t.Fatal("no preflight demand for the backlog repository")
	}
	counts := make(map[string]int)
	for _, use := range demand.labelUses {
		counts[use.label]++
	}
	for _, label := range []string{providers.LabelReady, providers.LabelClaimed, needsRemediationLabel, providers.StatusLabelFor(providers.WorkItemStatusDone)} {
		if counts[label] == 0 {
			t.Errorf("implicit lifecycle label %q is invisible to preflight", label)
		}
	}
	if counts[providers.LabelClaimed] != 1 || counts[needsRemediationLabel] != 1 {
		t.Fatalf("explicit and implicit uses were double-reported: %v", counts)
	}
}

func TestReservedLabelPreflightProviderEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		want      string
		forbidden string
	}{
		{"missing", http.StatusOK, `[{"name":"goobers:approved"}]`, `"goobers:ready", which does not exist`, "existence is unknown"},
		{"present case insensitive", http.StatusOK, `[{"name":"GOOBERS:READY"}]`, "", "WARNING"},
		{"inaccessible", http.StatusNotFound, `{"message":"not found"}`, "label existence is unknown, not missing", "which does not exist"},
		{"denied", http.StatusForbidden, `{"message":"denied"}`, "label existence is unknown, not missing", "which does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/backlog/labels" {
					t.Errorf("unexpected preflight request: %s %s", r.Method, r.URL)
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			old := targetRepositoryLabels
			t.Cleanup(func() { targetRepositoryLabels = old })
			targetRepositoryLabels = func(ctx context.Context, repo instance.RepoRef, token string) ([]string, error) {
				provider := providers.NewGitHubProvider(token, func(p *providers.GitHubProvider) { p.BaseURL = server.URL }, providers.WithMaxTransientRetries(0))
				return provider.RepositoryLabelNames(ctx, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name})
			}
			var output bytes.Buffer
			diagnostics := &diagnosticCollector{}
			checkGitHubRepositoryReality("repos[0] acme/backlog", instance.RepoRef{Provider: "github", Owner: "acme", Name: "backlog"}, "test-token",
				&repoRealityDemand{labelUses: []labelUse{{label: providers.LabelReady, kind: labelUseApply, where: "Workflow/curate lifecycle", file: "curate.yaml"}}}, &output, diagnostics)
			if requests.Load() != 1 || !strings.Contains(output.String(), tc.want) || strings.Contains(output.String(), tc.forbidden) {
				t.Fatalf("requests=%d output=%q", requests.Load(), output.String())
			}
			if tc.want != "" && !strings.Contains(output.String(), "acme/backlog") {
				t.Fatal("finding lost repository identity")
			}
			if tc.status != http.StatusOK && (len(diagnostics.findings) != 1 || diagnostics.findings[0].Code != "REPOLABEL001") {
				t.Fatalf("inaccessible repository must have a machine-readable finding: %+v", diagnostics.findings)
			}
		})
	}
}
