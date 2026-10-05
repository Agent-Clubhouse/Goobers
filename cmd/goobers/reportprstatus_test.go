package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestParseStatusState(t *testing.T) {
	cases := []struct {
		input string
		want  providers.CheckState
	}{
		{"succeeded", providers.CheckStatePassing},
		{"success", providers.CheckStatePassing},
		{"passing", providers.CheckStatePassing},
		{"failed", providers.CheckStateFailing},
		{"failure", providers.CheckStateFailing},
		{"failing", providers.CheckStateFailing},
		{"pending", providers.CheckStatePending},
		{"", providers.CheckStatePending},
	}
	for _, tc := range cases {
		got, err := parseStatusState(tc.input)
		if err != nil {
			t.Fatalf("parseStatusState(%q) error: %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("parseStatusState(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
	if got, err := parseStatusState("bogus"); got != "" || err == nil || err.Error() != `unknown status state "bogus" (want succeeded|failed|pending)` {
		t.Fatalf("parseStatusState(bogus) = %q, %v", got, err)
	}
}

// report-pr-status is an Azure DevOps parity feature: a GitHub-routed run must
// fail with an actionable error rather than silently succeeding.
func TestReportPRStatusRejectsGitHubProvider(t *testing.T) {
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")
	t.Setenv(executor.InputEnvVar("prNumber"), "42")

	var stdout, stderr bytes.Buffer
	code := runReportPRStatus([]string{t.TempDir()}, &stdout, &stderr)
	if code != 1 || stdout.String() != "" || stderr.String() != "error: provider \"github\" does not support publishing a policy-gate-able PR status (Azure DevOps and Gitea only, #772)\n" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestReportPRStatusRequiresPRNumber(t *testing.T) {
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderADO))
	t.Setenv(executor.RepoOwnerEnvVar, "example-org")
	t.Setenv(executor.RepoProjectEnvVar, "Example Service")
	t.Setenv(executor.RepoNameEnvVar, "Example.Repo")

	var stdout, stderr bytes.Buffer
	code := runReportPRStatus([]string{t.TempDir()}, &stdout, &stderr)
	if code != 1 || stdout.String() != "" || stderr.String() != "error: prNumber is required (wire it from open-pr via inputsFrom)\n" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

// TestReportPRStatusHeadShaPinsTheStatus proves the optional headSha input
// reaches the provider: on Azure DevOps a status whose evidence covers an
// older commit than the pull request's latest iteration is refused, and one
// that covers the latest iteration is posted.
func TestReportPRStatusHeadShaPinsTheStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		headSha  string
		wantCode int
	}{
		{name: "reviewed head is current", headSha: "current-head", wantCode: 0},
		{name: "head moved after the evidence", headSha: "reviewed-head", wantCode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			setNonGitHubStageEnv(t, providers.ProviderADO)
			t.Setenv(executor.InputEnvVar("prNumber"), "77")
			t.Setenv(executor.InputEnvVar("headSha"), tc.headSha)
			t.Setenv(executor.InputEnvVar("resultFile"), filepath.Join(t.TempDir(), "status-result.json"))
			posted := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pullrequests/77/iterations"):
					_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{
						{"id": 1, "sourceRefCommit": map[string]string{"commitId": "reviewed-head"}},
						{"id": 2, "sourceRefCommit": map[string]string{"commitId": "current-head"}},
					}})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pullrequests/77/iterations/2/statuses"):
					posted = true
					_ = json.NewEncoder(w).Encode(map[string]int{"id": 11})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			pointADOStageProviderAt(t, server)

			code, _, stderr := runArgs(t, "report-pr-status", root)
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d; stderr = %q", code, tc.wantCode, stderr)
			}
			if posted != (tc.wantCode == 0) {
				t.Fatalf("status posted = %v, want %v", posted, tc.wantCode == 0)
			}
			if tc.wantCode != 0 && !strings.Contains(stderr, "head moved from reviewed-head to current-head") {
				t.Fatalf("stderr = %q, want the head-moved refusal", stderr)
			}
		})
	}
}

func TestReportPRStatusPublishesExactRequestAndResultContract(t *testing.T) {
	root := initDemo(t)
	setNonGitHubStageEnv(t, providers.ProviderADO)
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "custom-status.json")
	inputs := map[string]string{
		"prNumber":         "77",
		"statusGenre":      "quality",
		"statusName":       "review-and-test",
		"state":            "failure",
		"description":      "review rejected",
		"targetUrl":        "https://runs.example/123",
		"pull-request-url": "https://pulls.example/77",
		"headSha":          "reviewed-head",
		"resultFile":       resultFile,
	}
	for key, value := range inputs {
		t.Setenv(executor.InputEnvVar(key), value)
	}

	var requests []string
	var postedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Basic Z29vYmVyczpwci10b2tlbg==" {
			t.Errorf("Authorization = %q, want the github:pr:write credential", got)
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pullrequests/77/iterations"):
			_, _ = io.WriteString(w, `{"value":[{"id":4,"sourceRefCommit":{"commitId":"reviewed-head"}}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pullrequests/77/iterations/4/statuses"):
			postedBody = body
			_, _ = io.WriteString(w, `{"id":31}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	pointADOStageProviderAt(t, server)

	code, stdout, stderr := runArgs(t, "report-pr-status", root)
	if code != 0 || stdout != "published pr #77 status quality/review-and-test = failing (id 31)\n" || stderr != "" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	wantRequests := []string{
		"GET /acme/project/_apis/git/repositories/web/pullrequests/77/iterations?api-version=7.1",
		"POST /acme/project/_apis/git/repositories/web/pullrequests/77/iterations/4/statuses?api-version=7.1",
	}
	if fmt.Sprint(requests) != fmt.Sprint(wantRequests) {
		t.Fatalf("requests = %q, want %q", requests, wantRequests)
	}
	if got, want := string(postedBody), `{"context":{"genre":"quality","name":"review-and-test"},"description":"review rejected","state":"failed","targetUrl":"https://runs.example/123"}`; got != want {
		t.Fatalf("published JSON = %s, want %s", got, want)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"integrity":"unapproved","prNumber":"77","state":"failing","statusGenre":"quality","statusId":"31","statusName":"review-and-test"}`; got != want {
		t.Fatalf("result bytes = %s, want %s", got, want)
	}
}

func TestReportPRStatusDefaultsAndInputPrecedence(t *testing.T) {
	root := initDemo(t)
	setNonGitHubStageEnv(t, providers.ProviderADO)
	workDir := t.TempDir()
	t.Chdir(workDir)
	t.Setenv(executor.InputEnvVar("resultFile"), "")
	t.Setenv(executor.InputEnvVar("prNumber"), "19")
	t.Setenv(executor.InputEnvVar("pull-request-url"), "https://pulls.example/19")

	var postedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"value":[{"id":2}]}`)
		case http.MethodPost:
			postedBody = body
			_, _ = io.WriteString(w, `{"id":8}`)
		}
	}))
	t.Cleanup(server.Close)
	pointADOStageProviderAt(t, server)

	code, stdout, stderr := runArgs(t, "report-pr-status", root)
	if code != 0 || stdout != "published pr #19 status goobers/validation = passing (id 8)\n" || stderr != "" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got, want := string(postedBody), `{"context":{"genre":"goobers","name":"validation"},"description":"goobers validation passed (review + local CI)","state":"succeeded","targetUrl":"https://pulls.example/19"}`; got != want {
		t.Fatalf("published JSON = %s, want %s", got, want)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "status-result.json"))
	if err != nil {
		t.Fatalf("read default result filename: %v", err)
	}
	if got, want := string(data), `{"integrity":"unapproved","prNumber":"19","state":"passing","statusGenre":"goobers","statusId":"8","statusName":"validation"}`; got != want {
		t.Fatalf("result bytes = %s, want %s", got, want)
	}
}

func TestReportPRStatusValidationAndProviderConstructionOrder(t *testing.T) {
	root := initDemo(t)
	setNonGitHubStageEnv(t, providers.ProviderADO)
	t.Setenv(executor.InputEnvVar("prNumber"), "77")
	t.Setenv(executor.InputEnvVar("state"), "bogus")
	t.Setenv(executor.CredentialEnvVar("github:pr:write"), "")

	code, stdout, stderr := runArgs(t, "report-pr-status", root)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "GOOBERS_CRED_GITHUB_PR_WRITE") || strings.Contains(stderr, "unknown status state") {
		t.Fatalf("missing credential: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	t.Setenv(executor.CredentialEnvVar("github:pr:write"), "pr-token")
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	t.Cleanup(server.Close)
	pointADOStageProviderAt(t, server)
	code, stdout, stderr = runArgs(t, "report-pr-status", root)
	if code != 1 || stdout != "" || stderr != "error: unknown status state \"bogus\" (want succeeded|failed|pending)\n" {
		t.Fatalf("invalid state: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if requests != 0 {
		t.Fatalf("provider requests = %d, want none after validation rejection", requests)
	}
}

func TestReportPRStatusProviderFailureWritesTypedArtifact(t *testing.T) {
	root := initDemo(t)
	setNonGitHubStageEnv(t, providers.ProviderADO)
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "provider-error.json")
	t.Setenv(executor.InputEnvVar("resultFile"), resultFile)
	t.Setenv(executor.InputEnvVar("prNumber"), "77")

	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"value":[{"id":3}]}`)
			return
		}
		posts++
		http.Error(w, `{"message":"temporarily unavailable"}`, http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	pointADOStageProviderAt(t, server)

	code, stdout, stderr := runArgs(t, "report-pr-status", root)
	providerError := "publish pull request status: POST " + server.URL +
		`/acme/project/_apis/git/repositories/web/pullrequests/77/iterations/3/statuses?api-version=7.1 failed: status 503: {"message":"temporarily unavailable"}`
	if code != 1 || stdout != "" || stderr != "error: "+providerError+"\n" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if posts != 1 {
		t.Fatalf("publication calls = %d, want 1", posts)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	wantResult := `{"errorCode":"github_server_error","errorMessage":` +
		strconv.Quote(providerError) + `,"errorRetryable":true,"integrity":"unapproved"}`
	if string(data) != wantResult {
		t.Fatalf("typed provider result = %s, want %s", data, wantResult)
	}
}

func TestReportPRStatusCancellationDoesNotPublish(t *testing.T) {
	root := initDemo(t)
	setNonGitHubStageEnv(t, providers.ProviderADO)
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "canceled.json")
	t.Setenv(executor.InputEnvVar("resultFile"), resultFile)
	t.Setenv(executor.InputEnvVar("prNumber"), "77")
	t.Setenv(executor.InputEnvVar(executor.InputTimeout), "100ms")

	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	pointADOStageProviderAt(t, server)

	start := time.Now()
	code, stdout, stderr := runArgs(t, "report-pr-status", root)
	const cancellationError = "publish pull request status: context deadline exceeded"
	if code != 1 || stdout != "" || stderr != "error: "+cancellationError+"\n" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond || elapsed > time.Second {
		t.Fatalf("cancellation elapsed = %s", elapsed)
	}
	if posts != 0 {
		t.Fatalf("publication calls = %d, want none when iteration lookup is canceled", posts)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	wantResult := `{"errorCode":"provider_error","errorMessage":"` + cancellationError +
		`","errorRetryable":false,"integrity":"unapproved"}`
	if string(data) != wantResult {
		t.Fatalf("typed cancellation result = %s, want %s", data, wantResult)
	}
}

// The post-success filesystem boundary cannot be injected independently yet:
// this fixture changes the result path only after the fake provider accepts the
// publication. It freezes the current no-republish behavior for later seam work.
func TestReportPRStatusResultWriteFailureDoesNotRepublish(t *testing.T) {
	root := initDemo(t)
	setNonGitHubStageEnv(t, providers.ProviderADO)
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "becomes-a-directory")
	t.Setenv(executor.InputEnvVar("resultFile"), resultFile)
	t.Setenv(executor.InputEnvVar("prNumber"), "77")

	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"value":[{"id":3}]}`)
			return
		}
		posts++
		if err := os.Mkdir(resultFile, 0o755); err != nil {
			t.Errorf("replace result path with directory: %v", err)
		}
		_, _ = io.WriteString(w, `{"id":12}`)
	}))
	t.Cleanup(server.Close)
	pointADOStageProviderAt(t, server)

	code, stdout, stderr := runArgs(t, "report-pr-status", root)
	writeError := directoryWriteError(resultFile)
	wantStderr := "error: write " + resultFile + ": " + writeError + "\n" +
		"warning: write provider-stage result " + resultFile + ": write typed result: " + writeError + "\n"
	if code != 1 || stdout != "" || stderr != wantStderr {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if posts != 1 {
		t.Fatalf("publication calls = %d, want exactly 1 after result-write failure", posts)
	}
}

func directoryWriteError(path string) string {
	return (&os.PathError{Op: "open", Path: path, Err: syscall.EISDIR}).Error()
}
