package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

func TestCredentialResolveSelectedRevisionContract(t *testing.T) {
	for _, test := range []struct {
		name, revision string
		status         int
	}{
		{"valid", fmt.Sprintf(`{"repository":{"provider":"github","owner":"acme","name":"web","url":"https://github.com/acme/web"},"commitSha":%q}`, strings.Repeat("a", 40)), http.StatusOK},
		{"short-sha", `{"repository":{"provider":"github","owner":"acme","name":"web","url":"https://github.com/acme/web"},"commitSha":"abc"}`, http.StatusBadRequest},
		{"unknown-authority", fmt.Sprintf(`{"repository":{"provider":"github","owner":"acme","name":"web","url":"https://github.com/acme/web","credential":"write"},"commitSha":%q}`, strings.Repeat("a", 40)), http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeCredentialService{}
			handler := writePlaneHandler(t, podAuthenticator(), AllowAll, WithCredentialService(service))
			response := httptest.NewRecorder()
			body := fmt.Sprintf(`{"runId":"run-1","stage":"inspect","workspaceRevision":%s}`, test.revision)
			handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.CredentialResolvePath, body))
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body)
			}
			if test.status == http.StatusOK {
				if len(service.requests) != 1 || service.requests[0].WorkspaceRevision == nil || service.requests[0].WorkspaceRevision.CommitSHA != strings.Repeat("a", 40) {
					t.Fatalf("revision was not transported intact: %+v", service.requests)
				}
			} else if len(service.requests) != 0 {
				t.Fatal("malformed authority reached the credential service")
			}
		})
	}
}
