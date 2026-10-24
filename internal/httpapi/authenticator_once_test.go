package httpapi

import (
	"io"
	"log"
	"strings"
	"testing"
)

func TestASecondAuthenticatorIsRefusedNotSilentlyReplaced(t *testing.T) {
	_, err := NewHandler(&fakeReader{}, RequireRoles(), log.New(io.Discard, "", 0), WithAuthenticator(NullAuthenticator{}), WithAuthenticator(DenyAllAuthenticator{}))
	if err == nil || !strings.Contains(err.Error(), "authenticator is already set") {
		t.Fatalf("second authenticator = %v, want refused", err)
	}
	if _, err := NewHandler(&fakeReader{}, RequireRoles(), log.New(io.Discard, "", 0), WithAuthenticator(DenyAllAuthenticator{})); err != nil {
		t.Fatalf("single authenticator refused: %v", err)
	}
}
