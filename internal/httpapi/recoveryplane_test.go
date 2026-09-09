package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type recoveryServiceFunc func(context.Context, string, string, string, io.Writer) error

func (f recoveryServiceFunc) StreamRecovery(ctx context.Context, run, key, issue string, out io.Writer) error {
	return f(ctx, run, key, issue, out)
}

func TestRecoveryRouteEnforcesRunScopeAndOperateRole(t *testing.T) {
	for _, mode := range []string{"claims", "journal", "blob", "foreign", "view", "operate", "refused"} {
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
			if called != (want == http.StatusOK || mode == "refused") {
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
