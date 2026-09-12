package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recoveryPublisherFunc func(context.Context, string, string, string, io.Reader) error

func (f recoveryPublisherFunc) PublishRecovery(ctx context.Context, run, key, issue string, body io.Reader) error {
	return f(ctx, run, key, issue, body)
}
func (recoveryPublisherFunc) StreamRecovery(context.Context, string, string, string, io.Writer) error {
	return errors.New("download not used")
}

func TestRecoveryPublicationEnforcesOwnRunClaimsAndDurableAck(t *testing.T) {
	for _, mode := range []string{"claims", "journal", "blob", "foreign", "view", "operate", "refused", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			principal := &Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer, Scopes: []string{ScopeClaims}}
			want := http.StatusNoContent
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
			case "oversize":
				want = http.StatusRequestEntityTooLarge
			}
			called := false
			service := recoveryPublisherFunc(func(_ context.Context, run, key, issue string, body io.Reader) error {
				called = true
				if run != "run-1" || key != "repo" || issue != "7" {
					t.Fatal("publication identity changed")
				}
				if mode == "refused" {
					return errors.New("private credential-bearing detail")
				}
				data, err := io.ReadAll(body)
				if err != nil || string(data) != "archive\x00bytes" {
					t.Fatalf("binary body changed: %q %v", data, err)
				}
				return nil
			})
			handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: principal}), WithRecoveryService(service))
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/run-1/recovery?repositoryKey=repo&issue=7", strings.NewReader("archive\x00bytes"))
			if mode == "oversize" {
				request.ContentLength = maxRecoveryUploadBytes + 1
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != want || called != (want == http.StatusNoContent || mode == "refused") {
				t.Fatalf("status=%d called=%t body=%s", response.Code, called, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "credential-bearing") {
				t.Fatal("leaked publication error")
			}
		})
	}
}
