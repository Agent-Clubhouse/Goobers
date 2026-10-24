package podauth

import (
	"net/http/httptest"
	"testing"
)

func TestAnUninitializedAuthRefusesInsteadOfPanicking(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/runs", nil)
	request.Header.Set("Authorization", "Bearer goobers-pod.x")
	for name, auth := range map[string]*Auth{"zero": {}, "nil": nil} {
		if principal, err := auth.Authenticate(request); err == nil || principal != nil {
			t.Fatalf("%s auth = %v, %v; want a refusal", name, principal, err)
		}
	}
}
