package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestRemediationDemandCounterCountsOnlyUnclaimedEligiblePRs(t *testing.T) {
	const token = "remediation-counter-token"
	prs := []map[string]interface{}{
		{
			"number": 41, "state": "open",
			"head":   map[string]string{"ref": "team/implementation/41", "sha": "sha-41"},
			"base":   map[string]string{"ref": "main", "sha": "base"},
			"labels": labelsJSON([]string{needsRemediationLabel}),
		},
		{
			"number": 42, "state": "open",
			"head":   map[string]string{"ref": "team/implementation/42", "sha": "sha-42"},
			"base":   map[string]string{"ref": "main", "sha": "base"},
			"labels": labelsJSON(nil),
		},
		{
			"number": 43, "state": "open",
			"head":   map[string]string{"ref": "team/implementation/43", "sha": "sha-43"},
			"base":   map[string]string{"ref": "main", "sha": "base"},
			"labels": labelsJSON(nil),
		},
		{
			"number": 44, "state": "open",
			"head":   map[string]string{"ref": "outside/44", "sha": "sha-44"},
			"base":   map[string]string{"ref": "main", "sha": "base"},
			"labels": labelsJSON(nil),
		},
		{
			"number": 45, "state": "open",
			"head":   map[string]string{"ref": "team/implementation/45", "sha": "sha-45"},
			"base":   map[string]string{"ref": "main", "sha": "base"},
			"labels": labelsJSON([]string{providers.LabelNeedsHuman}),
		},
	}
	checkStates := map[string]string{
		"sha-41": "failure",
		"sha-42": "failure",
		"sha-43": "success",
		"sha-45": "failure",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/web/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("base") != "main" {
			t.Fatalf("base query = %q, want main", r.URL.Query().Get("base"))
		}
		writeFakeJSON(w, prs)
	})
	for sha, state := range checkStates {
		sha, state := sha, state
		mux.HandleFunc("/repos/acme/web/commits/"+sha+"/status", func(w http.ResponseWriter, _ *http.Request) {
			writeFakeJSON(w, map[string]interface{}{
				"state": state,
				"statuses": []map[string]string{{
					"context": "required-ci",
					"state":   state,
				}},
			})
		})
		mux.HandleFunc("/repos/acme/web/commits/"+sha+"/check-runs", func(w http.ResponseWriter, _ *http.Request) {
			writeFakeJSON(w, map[string]interface{}{"check_runs": []interface{}{}})
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			http.Error(w, "wrong token", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(provider *providers.GitHubProvider) {
			provider.BaseURL = server.URL
		})...)
	}
	t.Cleanup(func() { newGitHubProvider = previous })

	t.Setenv("REMEDIATION_COUNTER_TOKEN", token)
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{
		Name: "acme/web",
		Env:  "REMEDIATION_COUNTER_TOKEN",
	}})
	if err != nil {
		t.Fatal(err)
	}
	root := initDemo(t)
	schedulerDir := layoutFor(root).SchedulerDir()
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.ClaimScoped(localscheduler.ClaimKey{
		Gaggle:     "goobers",
		Provider:   string(providers.ProviderGitHub),
		ExternalID: pullRequestClaimKey(41),
	}, "active-run", "pr-remediation", time.Hour); err != nil || !ok {
		t.Fatalf("seed active claim: ok=%t err=%v", ok, err)
	}

	counter := &remediationDemandCounter{
		ref:          "acme/web",
		repo:         providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
		base:         "main",
		headPrefix:   "team/",
		gaggle:       "goobers",
		resolver:     resolver,
		reg:          &backlogTestRegistrar{},
		schedulerDir: schedulerDir,
		now:          func() time.Time { return time.Now() },
	}
	count, err := counter.EligibleCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("eligible count = %d, want only unclaimed failing PR #42", count)
	}
}

func TestFilterClaimAvailablePullRequestsSurfacesLedgerErrors(t *testing.T) {
	schedulerDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(schedulerDir, claimLedgerFileName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := filterClaimAvailablePullRequests(
		schedulerDir,
		"goobers",
		providers.ProviderGitHub,
		"",
		[]providers.PullRequestSummary{{Number: 1}},
		time.Now(),
	)
	if err == nil {
		t.Fatal("count succeeded with a malformed claim ledger")
	}
	if err.Error() == "" {
		t.Fatal("count returned an empty error")
	}
}

var _ localscheduler.BacklogCounter = (*remediationDemandCounter)(nil)
var _ localscheduler.ProviderQuotaGuardedBacklogCounter = (*remediationDemandCounter)(nil)
