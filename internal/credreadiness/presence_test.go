package credreadiness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
)

func TestPresenceNeverResolvesSecrets(t *testing.T) {
	t.Setenv("PRESENCE_TOKEN", "opaque-secret-do-not-read")
	file := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(file, []byte("opaque-secret-do-not-read"), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		ref  credentials.TokenRef
		want Status
	}{
		{"env", credentials.TokenRef{Env: "PRESENCE_TOKEN"}, StatusPresent},
		{"file", credentials.TokenRef{File: file}, StatusPresent},
		{"missing-file", credentials.TokenRef{File: file + "-missing"}, StatusAbsent},
		{"directory", credentials.TokenRef{File: filepath.Dir(file)}, StatusUnobservable},
		{"keychain", credentials.TokenRef{Keychain: "service"}, StatusUnobservable},
		{"store", credentials.TokenRef{Store: "vault/model"}, StatusUnobservable},
		{"github-cli", credentials.TokenRef{GitHubCLI: &credentials.GitHubCLIRef{Hostname: "github.com", User: "operator"}}, StatusUnobservable},
		{"unknown", credentials.TokenRef{}, StatusUnsupportedSource},
		{"ambiguous", credentials.TokenRef{Env: "PRESENCE_TOKEN", Store: "vault/model"}, StatusUnsupportedSource},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := Presence(context.Background(), "agent:model", tt.ref, nil)
			if check.Status != tt.want {
				t.Fatalf("got %+v, want %s", check, tt.want)
			}
			if check.Status.Asserted() {
				t.Fatal("presence must not assert usability")
			}
			if strings.Contains(fmt.Sprint(check), "opaque-secret") {
				t.Fatal("secret leaked")
			}
		})
	}
}

type fakeMetadataStore struct {
	calls int
	err   error
}

func (s *fakeMetadataStore) inspect(_ context.Context, kind SourceKind, name string) (bool, error) {
	s.calls++
	if kind != SourceStore || name != "vault/model" {
		panic("wrong metadata scope")
	}
	return true, s.err
}
func TestPresenceMetadataStoreErrorsAreNotEmitted(t *testing.T) {
	store := &fakeMetadataStore{}
	ref := credentials.TokenRef{Store: "vault/model"}
	check := Presence(context.Background(), "agent:model", ref, store.inspect)
	if check.Status != StatusPresent || store.calls != 1 {
		t.Fatalf("%+v calls=%d", check, store.calls)
	}
	store.err = errors.New("backend returned opaque-secret")
	check = Presence(context.Background(), "agent:model", ref, store.inspect)
	if check.Status != StatusUnobservable || strings.Contains(check.Detail, "opaque-secret") {
		t.Fatalf("%+v", check)
	}
}
