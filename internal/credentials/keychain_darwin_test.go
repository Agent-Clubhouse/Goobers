//go:build darwin

package credentials

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestResolverResolvesFromKeychain(t *testing.T) {
	original := runSecurity
	t.Cleanup(func() { runSecurity = original })

	var gotArgs []string
	runSecurity = func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte("  keychain-secret\n"), nil
	}

	resolver, err := NewResolver([]TokenRef{{Name: "gh", Keychain: "goobers/github-issues"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	got, err := resolver.Resolve(context.Background(), "gh")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "keychain-secret" {
		t.Fatalf("Resolve = %q, want keychain-secret", got)
	}
	wantArgs := []string{"find-generic-password", "-s", "goobers/github-issues", "-w"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("security args = %q, want %q", gotArgs, wantArgs)
	}
}

func TestResolverKeychainFailuresFailClosed(t *testing.T) {
	original := runSecurity
	t.Cleanup(func() { runSecurity = original })

	runSecurity = func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("item not found")
	}
	resolver, err := NewResolver([]TokenRef{{Name: "gh", Keychain: "goobers/missing"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if _, err := resolver.Resolve(context.Background(), "gh"); err == nil {
		t.Fatal("Resolve: want Keychain error, got nil")
	}

	runSecurity = func(context.Context, ...string) ([]byte, error) {
		return []byte(" \n"), nil
	}
	_, err = resolver.Resolve(context.Background(), "gh")
	if !errors.Is(err, ErrTokenRefEmpty) {
		t.Fatalf("Resolve empty Keychain item = %v, want ErrTokenRefEmpty", err)
	}
}
