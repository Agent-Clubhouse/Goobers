package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

func interventionCLIFixture(t *testing.T, handler http.HandlerFunc) (string, *httptest.Server) {
	t.Helper()
	root := initDeterministicDemo(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(instance.NewLayout(root).SchedulerDir(), daemonAPIAddressFileName)
	if err := os.WriteFile(path, []byte(address.Host+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, server
}

func TestInterventionCLICommandsCallDaemonAPI(t *testing.T) {
	tests := []struct {
		name         string
		command      string
		actionPath   string
		flags        []string
		assertFields func(*testing.T, httpapi.InterventionRequest)
	}{
		{
			name:       "approve",
			command:    "approve",
			actionPath: "approve",
			flags:      []string{"--actor=operator", "--decision=pass"},
			assertFields: func(t *testing.T, input httpapi.InterventionRequest) {
				if input.Actor != "operator" || input.Decision != "pass" {
					t.Fatalf("input = %+v", input)
				}
			},
		},
		{
			name:       "override",
			command:    "override",
			actionPath: "override",
			flags:      []string{"--actor=operator", "--decision=pass", "--rationale=accepted risk"},
			assertFields: func(t *testing.T, input httpapi.InterventionRequest) {
				if input.Actor != "operator" || input.Decision != "pass" || input.Rationale != "accepted risk" {
					t.Fatalf("input = %+v", input)
				}
			},
		},
		{
			name:       "rerun stage",
			command:    "rerun-stage",
			actionPath: "rerun",
			flags:      []string{"--actor=operator", "--addendum=use the parser seam"},
			assertFields: func(t *testing.T, input httpapi.InterventionRequest) {
				if input.Actor != "operator" || input.InstructionAddendum != "use the parser seam" {
					t.Fatalf("input = %+v", input)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, _ := interventionCLIFixture(t, func(w http.ResponseWriter, request *http.Request) {
				wantPath := apicontract.V1Prefix + "/runs/run-1/stages/review/" + test.actionPath
				if request.Method != http.MethodPost || request.URL.Path != wantPath {
					t.Errorf("request = %s %s, want POST %s", request.Method, request.URL.Path, wantPath)
				}
				var input httpapi.InterventionRequest
				if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
					t.Error(err)
					return
				}
				test.assertFields(t, input)
				if request.Header.Get(httpapi.HeaderIdempotencyKey) == "" {
					t.Error("missing Idempotency-Key")
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(httpapi.InterventionResult{Phase: "running", State: "finish", JournalSeq: 12})
			})
			args := append([]string{test.command}, test.flags...)
			args = append(args, "run-1", "review", root)
			code, stdout, stderr := runArgs(t, args...)
			if code != 0 || stderr != "" {
				t.Fatalf("code = %d, stderr = %q", code, stderr)
			}
			if !strings.Contains(stdout, test.command+" accepted for run run-1") ||
				!strings.Contains(stdout, "phase=running state=finish") {
				t.Fatalf("stdout = %q", stdout)
			}
		})
	}
}

func TestInterventionCLIUsesBearerTokenAndReportsRefusal(t *testing.T) {
	t.Setenv("GOOBERS_API_TOKEN", "test-token-value")
	root, _ := interventionCLIFixture(t, func(w http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer test-token-value" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(apicontract.ErrorEnvelope{
			Error: apicontract.APIError{Code: "run_not_escalated", Message: "run is completed"},
		})
	})
	code, stdout, stderr := runArgs(t, "approve", "--actor=operator", "run-1", "review", root)
	if code != 1 || stdout != "" {
		t.Fatalf("code = %d, stdout = %q", code, stdout)
	}
	if !strings.Contains(stderr, "run_not_escalated: run is completed") ||
		strings.Contains(stderr, "test-token-value") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestInterventionCLIRequiresAuditText(t *testing.T) {
	code, _, stderr := runArgs(t, "override", "--actor=operator", "run-1", "review")
	if code != 2 || !strings.Contains(stderr, "--rationale is required") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	code, _, stderr = runArgs(t, "rerun-stage", "--actor=operator", "run-1", "implement")
	if code != 2 || !strings.Contains(stderr, "--addendum is required") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
}
