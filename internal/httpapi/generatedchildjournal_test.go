package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestGeneratedChildJournalRequiresSeparateOwnerAndOwnRun(t *testing.T) {
	for _, installed := range []bool{false, true} {
		ordinary, child := &fakeJournalService{}, &fakeJournalService{}
		auth := &fakeAuthenticator{principal: &Principal{Subject: podPrincipalSubject("run-1"), Issuer: GeneratedChildPrincipalIssuer, GeneratedChild: &GeneratedChildPrincipal{ContractDigest: journal.Digest(nil)}}}
		opts := []HandlerOption{WithJournalService(ordinary)}
		if installed {
			opts = append(opts, WithGeneratedChildJournalService(child))
		}
		handler := writePlaneHandler(t, auth, RequireRoles(), opts...)
		for _, run := range []string{"run-1", "run-2"} {
			before := len(child.requests)
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, jsonRequest(http.MethodPost, "/api/v1/runs/"+run+"/journal/emit", emitBody(run)))
			want := http.StatusForbidden
			if installed && run == "run-1" {
				want = http.StatusOK
			}
			if out.Code != want || len(ordinary.requests) != 0 || out.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatalf("journal owner selection: installed=%v run=%s code=%d ordinary=%d", installed, run, out.Code, len(ordinary.requests))
			}
			if want == http.StatusForbidden && len(child.requests) != before {
				t.Fatal("foreign run reached child owner")
			}
		}
	}
}
