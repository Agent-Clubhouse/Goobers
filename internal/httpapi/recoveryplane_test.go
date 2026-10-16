package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/recovery"
)

type recoveryServiceFunc func(context.Context, string, string, string, io.Writer) error

func (f recoveryServiceFunc) StreamRecovery(ctx context.Context, run, key, issue string, out io.Writer) error {
	return f(ctx, run, key, issue, out)
}

func TestRecoveryRouteEnforcesRunScopeAndOperateRole(t *testing.T) {
	for _, mode := range []string{"claims", "journal", "blob", "foreign", "view", "operate", "refused", "not-found"} {
		t.Run(mode, func(t *testing.T) {
			principal := &Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer, Scopes: []string{ScopeClaims}}
			want := http.StatusOK
			switch mode {
			case "journal", "blob":
				principal.Scopes = []string{mode}
				want = http.StatusForbidden
			case "foreign":
				principal.Subject = "run:another-run"
				want = http.StatusForbidden
			case "view", "operate":
				principal = &Principal{Subject: "human", Roles: []Role{Role(mode)}}
				if mode == "view" {
					want = http.StatusForbidden
				}
			case "refused":
				want = http.StatusForbidden
			case "not-found":
				want = http.StatusNotFound
			}
			called := false
			service := recoveryServiceFunc(func(_ context.Context, run, key, issue string, out io.Writer) error {
				called = true
				if run != "run-1" || key != "github|||team|repo|" || issue != "7" {
					t.Fatal("request identity changed")
				}
				if mode == "refused" {
					return errors.New("private service detail")
				}
				if mode == "not-found" {
					return recovery.ErrNoMatchingSnapshot
				}
				_, err := io.WriteString(out, "archive")
				return err
			})
			handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: principal}), WithRecoveryService(service))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/runs/run-1/recovery?repositoryKey=github%7C%7C%7Cteam%7Crepo%7C&issue=7", nil))
			if response.Code != want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, want, response.Body.String())
			}
			if called != (want == http.StatusOK || mode == "refused" || mode == "not-found") {
				t.Fatalf("unauthorized service call: %t", called)
			}
			if want == http.StatusOK && (response.Body.String() != "archive" || response.Header().Get("Cache-Control") != "no-store") {
				t.Fatal("archive response changed or permits caching")
			}
		})
	}
}

func TestRecoveryRouteAbortsPartialDelivery(t *testing.T) {
	service := recoveryServiceFunc(func(_ context.Context, _, _, _ string, out io.Writer) error {
		_, _ = io.WriteString(out, "partial")
		return errors.New("delivery interrupted")
	})
	request := httptest.NewRequest(http.MethodGet, "/?repositoryKey=repo&issue=7", nil)
	request.SetPathValue("run", "run-1")
	response := httptest.NewRecorder()
	defer func() {
		got := recover()
		err, ok := got.(error)
		if !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("partial stream did not abort: %v", got)
		}
		if response.Body.String() != "partial" {
			t.Fatal("JSON error appended to partial archive")
		}
	}()
	recoveryArchiveHandler(service, discardLogger()).ServeHTTP(response, request)
}

// TestRecoveryRouteCarriesSourceRunEvidenceOnlyWithArchive pins #5312: source
// run evidence reported before archive bytes reaches the receiver as headers,
// is withdrawn from a refusal, and cannot be reported once bytes have started.
func TestRecoveryRouteCarriesSourceRunEvidenceOnlyWithArchive(t *testing.T) {
	source := recovery.SourceRun{Workflow: "implementation", Phase: "escalated", TerminalSeq: 9}
	for _, mode := range []string{"delivered", "refused", "late"} {
		t.Run(mode, func(t *testing.T) {
			service := recoveryServiceFunc(func(_ context.Context, _, _, _ string, out io.Writer) error {
				reporter, ok := out.(recovery.SourceRunReporter)
				if !ok {
					t.Fatal("recovery stream cannot report source run evidence")
				}
				if mode == "late" {
					_, _ = io.WriteString(out, "archive")
				}
				reporter.ReportSourceRun(source)
				if mode == "refused" {
					return errors.New("refused after selection")
				}
				if mode == "delivered" {
					_, err := io.WriteString(out, "archive")
					return err
				}
				return nil
			})
			request := httptest.NewRequest(http.MethodGet, "/?repositoryKey=repo&issue=7", nil)
			request.SetPathValue("run", "run-1")
			response := httptest.NewRecorder()
			recoveryArchiveHandler(service, discardLogger()).ServeHTTP(response, request)
			got, err := recovery.SourceRunFromHeaders(response.Header())
			if err != nil {
				t.Fatal(err)
			}
			want := recovery.SourceRun{}
			if mode == "delivered" {
				want = source
			}
			if got != want {
				t.Fatalf("source run headers = %+v, want %+v (status %d)", got, want, response.Code)
			}
		})
	}
}

func TestRecoveryRouteReportsOverflowPending(t *testing.T) {
	service := recoveryServiceFunc(func(context.Context, string, string, string, io.Writer) error {
		return recovery.PendingPromotion(true, false, 0)
	})
	request := httptest.NewRequest(http.MethodGet, "/?repositoryKey=repo&issue=7", nil)
	request.SetPathValue("run", "run-1")
	response := httptest.NewRecorder()
	recoveryArchiveHandler(service, discardLogger()).ServeHTTP(response, request)
	var envelope ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusConflict || envelope.Error.Code != recovery.OverflowPendingCode || response.Header().Get(recovery.PromotionStateHeader) != recovery.PromotionCapacity {
		t.Fatalf("pending response: %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("pending headers: %v", response.Header())
	}
}

func TestRecoveryRoutesValidateScopeBeforeQueryAndReportAvailability(t *testing.T) {
	streamOnly := recoveryServiceFunc(func(context.Context, string, string, string, io.Writer) error {
		return nil
	})
	for _, test := range []struct {
		name    string
		method  string
		handler http.HandlerFunc
	}{
		{"delivery", http.MethodGet, recoveryArchiveHandler(nil, discardLogger())},
		{"publication", http.MethodPost, recoveryPublishHandler(streamOnly, discardLogger())},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/?repositoryKey=repo", nil)
			request.SetPathValue("run", "run-1")
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, Principal{
				Subject: "run:another-run",
				Issuer:  PodPrincipalIssuer,
			}))
			response := httptest.NewRecorder()
			test.handler.ServeHTTP(response, request)
			assertRecoveryError(t, response, http.StatusForbidden, "run_mismatch", "pod principal may only read its own run's recovery")

			for _, invalid := range []struct {
				name, run, target string
			}{
				{"invalid run", "../run", "/?repositoryKey=repo&issue=7"},
				{"missing key", "run-1", "/?issue=7"},
				{"oversized key", "run-1", "/?repositoryKey=" + strings.Repeat("r", 4097) + "&issue=7"},
				{"missing issue", "run-1", "/?repositoryKey=repo"},
				{"oversized issue", "run-1", "/?repositoryKey=repo&issue=" + strings.Repeat("7", 257)},
			} {
				t.Run(invalid.name, func(t *testing.T) {
					request = httptest.NewRequest(test.method, invalid.target, nil)
					request.SetPathValue("run", invalid.run)
					response = httptest.NewRecorder()
					test.handler.ServeHTTP(response, request)
					assertRecoveryError(t, response, http.StatusBadRequest, CodeInvalidRequest, "recovery requires bounded run, repository, and issue identities")
				})
			}

			request = httptest.NewRequest(test.method, "/?repositoryKey=repo&issue=7", nil)
			request.SetPathValue("run", "run-1")
			response = httptest.NewRecorder()
			test.handler.ServeHTTP(response, request)
			message := "recovery delivery is unavailable"
			if test.name == "publication" {
				message = "recovery publication is unavailable"
			}
			assertRecoveryError(t, response, http.StatusServiceUnavailable, "recovery_unavailable", message)
		})
	}
}

func assertRecoveryError(t *testing.T, response *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	var envelope ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != status || envelope.Error.Code != code || envelope.Error.Message != message {
		t.Fatalf("response = %d %+v, want %d %s %q", response.Code, envelope.Error, status, code, message)
	}
}
