package providers

import (
	"errors"
	"net/http"
	"testing"
)

func TestConfirmedMergeRefusalRequiresProviderRejection(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"transport", errors.New("PUT merge: unexpected EOF"), false},
		{"server error", &providerResponseError{method: http.MethodPut, statusCode: 500}, false},
		{"read conflict", &providerResponseError{method: http.MethodGet, statusCode: 409}, false},
		{"read not allowed", &providerResponseError{method: http.MethodGet, statusCode: 405, body: "not mergeable"}, false},
		{"merge refusal", &providerResponseError{method: http.MethodPut, statusCode: 405, body: "not mergeable"}, true},
		{"merge head mismatch", &providerResponseError{method: http.MethodPut, statusCode: 409}, true},
		{"policy refusal", PullRequestPolicyNotMetError{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsConfirmedMergeRefusal(tc.err); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
