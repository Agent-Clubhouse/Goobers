//go:build !darwin

package credentials

import (
	"context"
	"strings"
	"testing"
)

func TestResolverRejectsKeychainOnUnsupportedPlatform(t *testing.T) {
	resolver, err := NewResolver([]TokenRef{{Name: "gh", Keychain: "goobers/github-issues"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	_, err = resolver.Resolve(context.Background(), "gh")
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Resolve error = %v, want unsupported-platform error", err)
	}
}
