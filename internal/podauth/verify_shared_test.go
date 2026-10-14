package podauth

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
)

func TestEveryVerifierRejectsOverlongTokens(t *testing.T) {
	key, err := NewSignedKey([]byte(strings.Repeat("k", MinSignedKeyBytes)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	prefixes := []string{tokenPrefix, childPodTokenPrefix, WorkflowParentPodPrefix, workerTokenPrefix, workerBlobTokenPrefix, workerSurrenderTokenPrefix}
	for _, prefix := range prefixes {
		token := prefix + strings.Repeat("a", maxSignedTokenBytes) + ".x"
		request := httptest.NewRequest("GET", "/", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		if _, err := auth.Authenticate(request); !errors.Is(err, ErrMalformedToken) {
			t.Fatalf("%s: got %v, want ErrMalformedToken", prefix, err)
		}
	}
}

type onlyVerifier struct{ *Registry }

func TestUnsupportedVerifierFailsWithExplanation(t *testing.T) {
	auth, err := NewAuthenticator(onlyVerifier{NewRegistry()}, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{childPodTokenPrefix, WorkflowParentPodPrefix, workerTokenPrefix} {
		request := httptest.NewRequest("GET", "/", nil)
		request.Header.Set("Authorization", "Bearer "+prefix+"x.y")
		_, err := auth.Authenticate(request)
		if !errors.Is(err, ErrUnknownToken) || !strings.Contains(err.Error(), "does not support") {
			t.Fatalf("%s: got %v", prefix, err)
		}
	}
}
