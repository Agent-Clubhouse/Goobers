package adoauth

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

type sourceRunner struct {
	name string
	args []string
	out  []byte
}

func (r *sourceRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	return r.out, nil
}

func TestSourcePATReReadsEnvironment(t *testing.T) {
	t.Setenv("ADO_TEST_TOKEN", "first")
	source, err := Source(instance.RepoRef{
		Provider: "ado",
		Owner:    "org",
		Project:  "project",
		Name:     "repo",
		Token:    instance.TokenRef{Env: "ADO_TEST_TOKEN"},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := source.Credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADO_TEST_TOKEN", "second")
	second, err := source.Credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Secret != "first" || second.Secret != "second" {
		t.Fatalf("PAT rotation = %q then %q", first.Secret, second.Secret)
	}
}

type fakeStoreResolver map[string]string

func (f fakeStoreResolver) FetchSecret(_ context.Context, ref string) (string, error) {
	value, ok := f[ref]
	if !ok {
		return "", errStoreNotDeclared
	}
	return value, nil
}

var errStoreNotDeclared = errors.New("secretstore: not declared")

func TestSourcePATResolvesStoreRef(t *testing.T) {
	repo := instance.RepoRef{
		Provider: "ado",
		Owner:    "org",
		Project:  "project",
		Name:     "repo",
		Token:    instance.TokenRef{Store: "prod-kv/ado-pat"},
	}
	source, err := Source(repo, nil, fakeStoreResolver{"prod-kv/ado-pat": "kv-pat"})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := source.Credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if credential.Secret != "kv-pat" {
		t.Fatalf("credential.Secret = %q, want the store-resolved PAT", credential.Secret)
	}

	// Without a store resolver the same repo fails closed at construction —
	// a store-backed PAT must never degrade into an unconfigured one.
	if _, err := Source(repo, nil, nil); err == nil {
		t.Fatal("Source: want fail-closed error for store ref without a store resolver, got nil")
	}
}

func TestSourceAzureCLI(t *testing.T) {
	expires := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	runner := &sourceRunner{out: []byte(`{"accessToken":"entra","expires_on":` + expires + `}`)}
	source, err := Source(instance.RepoRef{
		Provider: "ado",
		Owner:    "org",
		Project:  "project",
		Name:     "repo",
		Auth:     &instance.ADOAuthConfig{Kind: instance.ADOAuthAzureCLI, Tenant: "tenant"},
	}, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := source.Credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if credential.Secret != "entra" || runner.name != "az" {
		t.Fatalf("credential = %#v, runner = %q %#v", credential, runner.name, runner.args)
	}
}
