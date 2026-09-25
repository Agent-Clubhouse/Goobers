package providers

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/goobers/goobers/internal/journal"
)

type adoAuthRunner struct {
	name string
	args []string
	env  []string
	out  []byte
	err  error
}

func (r *adoAuthRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	return r.out, r.err
}

func (r *adoAuthRunner) RunWithEnv(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	r.env = append([]string(nil), env...)
	return r.out, r.err
}

func TestAzureCLICredentialSourceCachesAndParsesToken(t *testing.T) {
	expires := time.Now().Add(time.Hour).Unix()
	runner := &adoAuthRunner{out: []byte(`{"accessToken":"entra-token","expires_on":` + strconv.FormatInt(expires, 10) + `}`)}
	source := NewAzureCLIADOCredentialSource(runner, "tenant-id")

	first, err := source.Credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != adoCredentialBearer || first.Secret != "entra-token" || second.Secret != first.Secret {
		t.Fatalf("credentials = %#v, %#v", first, second)
	}
	if runner.name != "az" || strings.Join(runner.args, " ") !=
		"account get-access-token --resource "+AzureDevOpsResourceID+" --output json --tenant tenant-id" {
		t.Fatalf("az invocation = %q %#v", runner.name, runner.args)
	}
}

func TestAzureCLICredentialSourceDoesNotEchoFailedOutput(t *testing.T) {
	runner := &adoAuthRunner{out: []byte(`{"accessToken":"must-not-leak"}`), err: errors.New("exit 1")}
	_, err := NewAzureCLIADOCredentialSource(runner, "").Credential(context.Background())
	if err == nil || strings.Contains(err.Error(), "must-not-leak") {
		t.Fatalf("Credential() error = %q", err)
	}
}

func TestParseAzureCLIExpiryTreatsNaiveTimestampAsLocalTime(t *testing.T) {
	original := time.Local
	local := time.FixedZone("test-local", 9*60*60)
	time.Local = local
	t.Cleanup(func() { time.Local = original })

	got, err := parseAzureCLIExpiry([]byte(`"2026-07-23 21:15:00.000000"`))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 23, 21, 15, 0, 0, local)
	if !got.Equal(want) || got.Location() != local {
		t.Fatalf("expiry = %s (%s), want %s (%s)", got, got.Location(), want, want.Location())
	}
}

type rotatingADOCredentialSource struct {
	token       string
	invalidated bool
}

func (s *rotatingADOCredentialSource) Credential(context.Context) (ADOCredential, error) {
	token := s.token
	if s.invalidated {
		token = "fresh-token"
	}
	return ADOCredential{Kind: adoCredentialBearer, Secret: token, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (s *rotatingADOCredentialSource) Invalidate() {
	s.invalidated = true
}

type adoAuthClient struct {
	headers []string
}

func (c *adoAuthClient) Do(req *http.Request) (*http.Response, error) {
	c.headers = append(c.headers, req.Header.Get("Authorization"))
	status := http.StatusOK
	if len(c.headers) == 1 {
		status = http.StatusUnauthorized
	}
	body := `{"id":42,"fields":{"System.WorkItemType":"Issue","System.Title":"item","System.State":"Active"}}`
	if strings.Contains(req.URL.Path, "/workitemtypes/") {
		body = `{"value":[{"name":"Active","category":"InProgress"}]}`
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

type adoHTTPClientFunc func(*http.Request) (*http.Response, error)

func (f adoHTTPClientFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestADOProviderRefreshesBearerOnceOnUnauthorized(t *testing.T) {
	source := &rotatingADOCredentialSource{token: "stale-token"}
	client := &adoAuthClient{}
	provider := NewADOProvider("org", "project", "",
		WithADOCredentialSource(source),
		func(p *ADOProvider) { p.Client = client },
	)

	if _, err := provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project", Name: "repo"}, "42"); err != nil {
		t.Fatal(err)
	}
	if len(client.headers) != 3 ||
		client.headers[0] == client.headers[1] ||
		client.headers[1] != client.headers[2] {
		t.Fatalf("Authorization headers did not refresh exactly once: %#v", client.headers)
	}
}

func TestADOProviderCloneUsesChildOnlyCredentialEnvironment(t *testing.T) {
	runner := &adoAuthRunner{}
	provider := NewADOProvider("org", "project", "",
		WithADOCredentialSource(NewADOPATCredentialSource("goobers", "ado-pat")),
		func(p *ADOProvider) { p.Runner = runner },
	)

	_, err := provider.CloneRepository(context.Background(), CloneRequest{
		Repository:  RepositoryRef{Name: "repo", Project: "project"},
		Destination: "dest",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range runner.args {
		if strings.Contains(arg, "ado-pat") {
			t.Fatalf("credential leaked into argv: %#v", runner.args)
		}
	}
	joined := strings.Join(runner.env, "\n")
	if !strings.Contains(joined, "GIT_CONFIG_VALUE_1=AUTHORIZATION: Basic ") ||
		!strings.Contains(joined, "GIT_CONFIG_KEY_1=http.https://dev.azure.com/org/project/_git/repo/.extraheader") ||
		!strings.Contains(joined, "GIT_TERMINAL_PROMPT=0") {
		t.Fatalf("git auth environment = %#v", runner.env)
	}
	// PAT (Basic) never gets the MSA passthrough header: it already works on
	// every org, and the header is undocumented and untested for Basic.
	if strings.Contains(joined, "GIT_CONFIG_COUNT=3") || strings.Contains(joined, adoForceMsaPassThroughHeader) {
		t.Fatalf("PAT git auth environment must not carry the passthrough header: %#v", runner.env)
	}
}

func TestADOProviderRepositoryReachableUsesChildOnlyCredentialEnvironment(t *testing.T) {
	runner := &adoAuthRunner{}
	provider := NewADOProvider("org", "project", "",
		WithADOCredentialSource(NewADOPATCredentialSource("goobers", "ado-pat")),
		func(p *ADOProvider) { p.Runner = runner },
	)
	if err := provider.RepositoryReachable(context.Background(), RepositoryRef{Name: "repo", Project: "project"}); err != nil {
		t.Fatal(err)
	}
	if runner.name != "git" || !strings.Contains(strings.Join(runner.args, " "), "ls-remote --heads") {
		t.Fatalf("git invocation = %q %#v", runner.name, runner.args)
	}
	for _, arg := range runner.args {
		if strings.Contains(arg, "ado-pat") {
			t.Fatalf("credential leaked into argv: %#v", runner.args)
		}
	}
}

func TestADOProviderRegistersDynamicBearerCredential(t *testing.T) {
	reg := journal.NewRegistryScrubber()
	provider := NewADOProvider("org", "project", "",
		WithADOCredentialSource(&rotatingADOCredentialSource{token: "dynamic-bearer"}),
		WithADOSecretRegistrar(reg),
	)
	provider.Client = adoHTTPClientFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"id":42,"fields":{"System.WorkItemType":"Issue","System.Title":"item","System.State":"Active"}}`
		if strings.Contains(req.URL.Path, "/workitemtypes/") {
			body = `{"value":[{"name":"Active","category":"InProgress"}]}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	if _, err := provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project", Name: "repo"}, "42"); err != nil {
		t.Fatal(err)
	}
	if got := string(reg.Scrub([]byte("Bearer dynamic-bearer"))); strings.Contains(got, "dynamic-bearer") {
		t.Fatalf("dynamic bearer was not scrubbed: %q", got)
	}

	// A bearer credential also gets the MSA passthrough header as a second
	// git extraheader slot, since Bearer needs it against a non-Entra-backed
	// org (or an MSA account) for git, not just REST.
	runner := &adoAuthRunner{}
	provider.Runner = runner
	if _, err := provider.CloneRepository(context.Background(), CloneRequest{
		Repository:  RepositoryRef{Name: "repo", Project: "project"},
		Destination: "dest",
	}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.env, "\n")
	if !strings.Contains(joined, "GIT_CONFIG_COUNT=3") ||
		!strings.Contains(joined, "GIT_CONFIG_KEY_2=http.https://dev.azure.com/org/project/_git/repo/.extraheader") ||
		!strings.Contains(joined, "GIT_CONFIG_VALUE_2="+adoForceMsaPassThroughHeader+": "+adoForceMsaPassThroughValue) {
		t.Fatalf("bearer git auth environment missing the passthrough header slot: %#v", runner.env)
	}
}

// ADOGitAuthEnvironment is the exported entry point for push, remediation,
// worktree and recovery Git auth, so its slot layout is pinned directly: a
// bearer credential gets three GIT_CONFIG slots (the third is the passthrough
// extraheader), a PAT keeps exactly two.
func TestADOGitAuthEnvironmentSlotLayoutByCredentialKind(t *testing.T) {
	const remote = "https://dev.azure.com/example-org/example-project/_git/repo"
	const scoped = "http." + remote + "/.extraheader"
	slots := func(t *testing.T, source ADOCredentialSource) map[string]string {
		t.Helper()
		env, err := ADOGitAuthEnvironment(context.Background(), source, nil, remote)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, entry := range env {
			if key, value, _ := strings.Cut(entry, "="); strings.HasPrefix(key, "GIT_CONFIG_") {
				got[key] = value
			}
		}
		return got
	}

	bearer := slots(t, &rotatingADOCredentialSource{token: "bearer-token"})
	if bearer["GIT_CONFIG_COUNT"] != "3" ||
		bearer["GIT_CONFIG_VALUE_1"] != "AUTHORIZATION: Bearer bearer-token" ||
		bearer["GIT_CONFIG_KEY_2"] != scoped ||
		bearer["GIT_CONFIG_VALUE_2"] != adoForceMsaPassThroughHeader+": "+adoForceMsaPassThroughValue {
		t.Fatalf("bearer slots = %#v", bearer)
	}

	pat := slots(t, NewADOPATCredentialSource("goobers", "pat-token"))
	if pat["GIT_CONFIG_COUNT"] != "2" || pat["GIT_CONFIG_KEY_1"] != scoped {
		t.Fatalf("PAT slots = %#v", pat)
	}
	if _, ok := pat["GIT_CONFIG_KEY_2"]; ok {
		t.Fatalf("PAT must not carry a passthrough slot: %#v", pat)
	}
}

type fakeAzureTokenCredential struct {
	scope string
}

func (c *fakeAzureTokenCredential) GetToken(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.scope = options.Scopes[0]
	return azcore.AccessToken{Token: "identity-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestAzureIdentityCredentialUsesAzureDevOpsScope(t *testing.T) {
	credential := &fakeAzureTokenCredential{}
	got, err := newAzureIdentityADOCredentialSource(credential).Credential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != "identity-token" || credential.scope != AzureDevOpsResourceID+"/.default" {
		t.Fatalf("credential = %#v, scope = %q", got, credential.scope)
	}
}

// TestADOCredentialScrubFormsCoverEveryWireForm pins the strings a registrar
// receives for one ADO credential: the raw secret and the Authorization value
// built from it, so a captured header is redacted as well as the bare token.
func TestADOCredentialScrubFormsCoverEveryWireForm(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("goobers:pat-secret-value"))
	for _, tc := range []struct {
		name       string
		credential ADOCredential
		want       []string
	}{
		{
			name:       "bearer",
			credential: ADOCredential{Kind: adoCredentialBearer, Secret: "entra-secret-value"},
			want:       []string{"entra-secret-value", "Bearer entra-secret-value"},
		},
		{
			name:       "pat",
			credential: ADOCredential{Kind: adoCredentialPAT, Secret: "pat-secret-value"},
			want:       []string{"pat-secret-value", basic, "Basic " + basic},
		},
		{
			name:       "unknown kind keeps the raw secret",
			credential: ADOCredential{Kind: "other", Secret: "opaque-secret-value"},
			want:       []string{"opaque-secret-value"},
		},
		{
			name:       "empty secret",
			credential: ADOCredential{Kind: adoCredentialBearer},
			want:       nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.credential.ScrubForms()
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("ScrubForms() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestADOGitAuthEnvironmentRegistersTheBasicHeader proves a PAT's Basic
// Authorization value is registered, so a captured git header is redacted.
func TestADOGitAuthEnvironmentRegistersTheBasicHeader(t *testing.T) {
	reg := journal.NewRegistryScrubber()
	source := NewADOPATCredentialSource("", "pat-secret-value")
	if _, err := ADOGitAuthEnvironment(context.Background(), source, reg, "https://dev.azure.com/example-org/example-project/_git/example-repo"); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("goobers:pat-secret-value"))
	got := string(reg.Scrub([]byte("AUTHORIZATION: Basic " + encoded)))
	if strings.Contains(got, encoded) {
		t.Fatalf("Basic header was not scrubbed: %q", got)
	}
}
