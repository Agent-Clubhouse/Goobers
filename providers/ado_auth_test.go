package providers

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/goobers/goobers/internal/journal"
)

type adoAuthRunner struct {
	calls int
	name  string
	args  []string
	env   []string
	out   []byte
	err   error
}

func (r *adoAuthRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls++
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
	for _, tenant := range []string{"", "tenant.example.test"} {
		t.Run("tenant="+tenant, func(t *testing.T) {
			expires := time.Now().Add(time.Hour).Unix()
			runner := &adoAuthRunner{out: []byte(`{"accessToken":"entra-token","expires_on":` + strconv.FormatInt(expires, 10) + `}`)}
			source := NewAzureCLIADOCredentialSource(runner, tenant)

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
			wantArgs := []string{"account", "get-access-token", "--resource", AzureDevOpsResourceID, "--output", "json"}
			if tenant != "" {
				wantArgs = append(wantArgs, "--tenant", tenant)
			}
			if runner.calls != 1 || runner.name != "az" || !slices.Equal(runner.args, wantArgs) {
				t.Fatalf("az invocation = %q %#v (%d calls), want %#v once", runner.name, runner.args, runner.calls, wantArgs)
			}
		})
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
	err   error
}

func (c *fakeAzureTokenCredential) GetToken(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.scope = options.Scopes[0]
	if c.err != nil {
		return azcore.AccessToken{}, c.err
	}
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

// TestADOGitAuthEnvironmentRegistersTheBasicHeader pins existing behaviour
// rather than covering new code: ADOGitAuthEnvironment already registered a
// PAT's Basic Authorization value, so a captured git header is redacted. It
// guards that registration now that ScrubForms states the same forms.
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

// TestADODeliveredCredentialSourceFailsClearlyAfterUnauthorized pins the
// stage-side contract for a credential the daemon handed a stage: it is sent
// in its delivered scheme, a 401 is not answered by resending it, and the
// request fails with ErrADODeliveredCredentialRejected naming where the value
// came from, never the value itself. The error keeps the 401 response, so it
// still classifies as an authentication failure (typed and as text), and the
// rejection does not stick: the next request sends the value again.
func TestADODeliveredCredentialSourceFailsClearlyAfterUnauthorized(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       string
		wantHeader string
	}{
		{name: "bearer", kind: ADOCredentialKindBearer, wantHeader: "Bearer delivered-secret-value"},
		{name: "basic", kind: ADOCredentialKindPAT, wantHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:delivered-secret-value"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := NewADODeliveredCredentialSource(tc.kind, "delivered-secret-value", "github:pr:write")
			if err != nil {
				t.Fatal(err)
			}
			var headers []string
			provider := NewADOProvider("org", "project", "",
				WithADOCredentialSource(source),
				func(p *ADOProvider) {
					p.Client = adoHTTPClientFunc(func(req *http.Request) (*http.Response, error) {
						headers = append(headers, req.Header.Get("Authorization"))
						return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("TF400813: not authorized"))}, nil
					})
				},
			)
			_, err = provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project", Name: "repo"}, "42")
			if !errors.Is(err, ErrADODeliveredCredentialRejected) {
				t.Fatalf("GetWorkItem error = %v, want ErrADODeliveredCredentialRejected", err)
			}
			if !strings.Contains(err.Error(), "github:pr:write") || !strings.Contains(err.Error(), "expired, revoked, or without access to this resource") {
				t.Fatalf("error %q does not name the capability and the cause", err)
			}
			if !strings.Contains(err.Error(), "status 401") || !strings.Contains(err.Error(), "TF400813") {
				t.Fatalf("error %q dropped the 401 response detail", err)
			}
			if strings.Contains(err.Error(), "delivered-secret-value") {
				t.Fatalf("error leaks the credential: %q", err)
			}
			if !IsAuthenticationError(err) || !IsAuthenticationError(errors.New(err.Error())) {
				t.Fatalf("IsAuthenticationError(%v) = false (typed or as text), want true", err)
			}
			if len(headers) != 1 || headers[0] != tc.wantHeader {
				t.Fatalf("Authorization headers = %q, want exactly one %q", headers, tc.wantHeader)
			}
			_, _ = provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project", Name: "repo"}, "43")
			if len(headers) != 2 || headers[1] != tc.wantHeader {
				t.Fatalf("Authorization headers after a second request = %q, want the value sent again", headers)
			}
		})
	}
}

// TestADODeliveredCredentialRejectionUsesTheDeliveredExpiry pins #5905: with
// the expiry the daemon delivered beside the value, a 401 at or after it is
// reported as an expired credential, and one before it as revoked or without
// access. Either way the error stays ErrADODeliveredCredentialRejected, keeps
// the 401 and classifies as an authentication failure.
func TestADODeliveredCredentialRejectionUsesTheDeliveredExpiry(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		expiresAt time.Time
		want      string
		notWant   string
	}{
		{name: "past expiry", expiresAt: now.Add(-time.Minute), want: "(expired at 2026-09-28T11:59:00Z)", notWant: "revoked"},
		{name: "at expiry", expiresAt: now, want: "(expired at 2026-09-28T12:00:00Z)", notWant: "revoked"},
		{name: "before expiry", expiresAt: now.Add(time.Hour), want: "(revoked or without access to this resource; it does not expire until 2026-09-28T13:00:00Z)", notWant: "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := NewADODeliveredCredentialSourceWithExpiry(ADOCredentialKindBearer, "delivered-secret-value", "github:pr:write", tc.expiresAt)
			if err != nil {
				t.Fatal(err)
			}
			provider := NewADOProvider("org", "project", "",
				WithADOCredentialSource(source),
				func(p *ADOProvider) {
					p.now = func() time.Time { return now }
					p.Client = adoHTTPClientFunc(func(*http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("TF400813: not authorized"))}, nil
					})
				},
			)
			_, err = provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project", Name: "repo"}, "42")
			if !errors.Is(err, ErrADODeliveredCredentialRejected) || !IsAuthenticationError(err) {
				t.Fatalf("GetWorkItem error = %v, want an authentication failure matching ErrADODeliveredCredentialRejected", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) || strings.Contains(msg, tc.notWant) {
				t.Fatalf("error %q, want it to contain %q and not %q", msg, tc.want, tc.notWant)
			}
			if !strings.Contains(msg, "github:pr:write") || !strings.Contains(msg, "status 401") || strings.Contains(msg, "delivered-secret-value") {
				t.Fatalf("error %q must name the capability, keep the 401 and not leak the value", msg)
			}
		})
	}
}

func TestNewADODeliveredCredentialSourceRejectsUnusableInput(t *testing.T) {
	if _, err := NewADODeliveredCredentialSource("other", "value", "repo:push"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := NewADODeliveredCredentialSource(ADOCredentialKindBearer, " ", "repo:push"); err == nil || !strings.Contains(err.Error(), "repo:push") {
		t.Fatalf("empty secret error = %v, want one naming repo:push", err)
	}
}

// TestADOProviderRefreshesBearerOnSignInPage pins that ADO's rejected-bearer
// answer — a redirect to its sign-in service, which a following client sees
// as a 203 HTML page — refreshes the credential exactly as a 401 does,
// instead of failing the call on a JSON decode of the HTML ("invalid
// character '<'").
func TestADOProviderRefreshesBearerOnSignInPage(t *testing.T) {
	for name, first := range map[string]func() *http.Response{
		"followed redirect (203 HTML)": func() *http.Response {
			h := make(http.Header)
			h.Set("Content-Type", "text/html; charset=utf-8")
			return &http.Response{StatusCode: http.StatusNonAuthoritativeInfo, Header: h, Body: io.NopCloser(strings.NewReader("<html><head><title>Azure DevOps Services | Sign In</title>"))}
		},
		"unfollowed redirect (302 to sign-in)": func() *http.Response {
			h := make(http.Header)
			h.Set("Location", "https://spsprodeus21.vssps.visualstudio.com/_signin?realm=dev.azure.com")
			return &http.Response{StatusCode: http.StatusFound, Header: h, Body: io.NopCloser(strings.NewReader("<html><head><title>Object moved</title>"))}
		},
	} {
		t.Run(name, func(t *testing.T) {
			source := &rotatingADOCredentialSource{token: "stale-token"}
			var headers []string
			client := adoHTTPClientFunc(func(req *http.Request) (*http.Response, error) {
				headers = append(headers, req.Header.Get("Authorization"))
				if len(headers) == 1 {
					return first(), nil
				}
				body := `{"id":42,"fields":{"System.WorkItemType":"Issue","System.Title":"item","System.State":"Active"}}`
				if strings.Contains(req.URL.Path, "/workitemtypes/") {
					body = `{"value":[{"name":"Active","category":"InProgress"}]}`
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			provider := NewADOProvider("org", "project", "",
				WithADOCredentialSource(source),
				func(p *ADOProvider) { p.Client = client },
			)
			if _, err := provider.GetWorkItem(context.Background(), RepositoryRef{Project: "project", Name: "repo"}, "42"); err != nil {
				t.Fatalf("sign-in page was not treated as a refreshable 401: %v", err)
			}
			if len(headers) < 2 || headers[0] == headers[1] {
				t.Fatalf("credential was not refreshed after the sign-in page: %#v", headers)
			}
		})
	}
}

// TestADOSignInResponseNormalization pins which responses count as ADO's
// rejected-credential sign-in answer: only a 203, or a redirect to the
// sign-in service — never an ordinary 2xx or an unrelated redirect.
func TestADOSignInResponseNormalization(t *testing.T) {
	cases := []struct {
		status   int
		location string
		want     int
	}{
		{http.StatusNonAuthoritativeInfo, "", http.StatusUnauthorized},
		{http.StatusFound, "https://spsprodeus21.vssps.visualstudio.com/_signin?realm=dev.azure.com", http.StatusUnauthorized},
		{http.StatusFound, "https://login.microsoftonline.com/common/oauth2/authorize", http.StatusUnauthorized},
		{http.StatusFound, "https://dev.azure.com/org/project/_apis/git/repositories/other", http.StatusFound},
		{http.StatusOK, "", http.StatusOK},
		{http.StatusNoContent, "", http.StatusNoContent},
	}
	for _, tc := range cases {
		h := make(http.Header)
		if tc.location != "" {
			h.Set("Location", tc.location)
		}
		resp := &http.Response{StatusCode: tc.status, Header: h}
		normalizeADOSignInResponse(resp)
		if resp.StatusCode != tc.want {
			t.Errorf("status %d location %q: got %d, want %d", tc.status, tc.location, resp.StatusCode, tc.want)
		}
	}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestADOPATCredentialSourceRefusesUnusableValues(t *testing.T) {
	resolveErr := errors.New("token file unreadable")
	cases := map[string]struct {
		source  ADOCredentialSource
		ctx     context.Context
		wantErr func(error) bool
	}{
		"cancelled context": {
			source:  NewADOPATCredentialSource("", "pat-value"),
			ctx:     cancelledContext(),
			wantErr: func(err error) bool { return errors.Is(err, context.Canceled) },
		},
		"nil resolver": {
			source:  NewResolvingADOPATCredentialSource("", nil),
			wantErr: func(err error) bool { return err.Error() == "ado PAT credential resolver is nil" },
		},
		"resolver fails": {
			source:  NewResolvingADOPATCredentialSource("", func(context.Context) (string, error) { return "", resolveErr }),
			wantErr: func(err error) bool { return errors.Is(err, resolveErr) },
		},
		"blank value": {
			source:  NewADOPATCredentialSource("", " \t"),
			wantErr: func(err error) bool { return err.Error() == "ado PAT credential is empty" },
		},
	}
	for name, tc := range cases {
		ctx := tc.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		got, err := tc.source.Credential(ctx)
		if err == nil || !tc.wantErr(err) || got != (ADOCredential{}) {
			t.Errorf("%s: Credential = %+v, %v", name, got, err)
		}
	}
	got, err := NewADOPATCredentialSource("", "pat-value").Credential(context.Background())
	if err != nil || got.Kind != ADOCredentialKindPAT || got.Secret != "pat-value" || got.Username != "goobers" {
		t.Errorf("Credential = %+v, %v; want the PAT under the historical goobers username", got, err)
	}
}

func TestADODeliveredCredentialSourceHonorsCancellation(t *testing.T) {
	source, err := NewADODeliveredCredentialSource(ADOCredentialKindBearer, "delivered", "repo:push")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := source.Credential(cancelledContext()); !errors.Is(err, context.Canceled) || got.Secret != "" {
		t.Fatalf("Credential = %+v, %v; want the cancellation and no value", got, err)
	}
}

// failingRefreshableToken is a delivered value whose re-resolve fails.
type failingRefreshableToken struct{ err error }

func (f failingRefreshableToken) Token(context.Context) (string, error) { return "", f.err }
func (failingRefreshableToken) Expiry() time.Time                       { return time.Time{} }
func (failingRefreshableToken) Invalidate()                             {}

func TestADORefreshingDeliveredCredentialSource(t *testing.T) {
	if _, err := NewADORefreshingDeliveredCredentialSource("other", "repo:push", &fakeRefreshableToken{current: "v"}); err == nil || err.Error() != `unsupported ado credential kind "other"` {
		t.Errorf("unknown kind error = %v", err)
	}
	if _, err := NewADORefreshingDeliveredCredentialSource(ADOCredentialKindBearer, "repo:push", nil); err == nil || !strings.Contains(err.Error(), "repo:push has no token") {
		t.Errorf("nil token error = %v, want one naming repo:push", err)
	}

	// A value that cannot be re-resolved ends the request as the delivered
	// credential's rejection: an authentication error naming the capability.
	refreshErr := errors.New("grant expired")
	failing, err := NewADORefreshingDeliveredCredentialSource(ADOCredentialKindPAT, "repo:push", failingRefreshableToken{err: refreshErr})
	if err != nil {
		t.Fatal(err)
	}
	_, err = failing.Credential(context.Background())
	if !errors.Is(err, ErrADODeliveredCredentialRejected) || !errors.Is(err, refreshErr) || !IsAuthenticationError(err) || !strings.Contains(err.Error(), "repo:push") {
		t.Fatalf("Credential error = %v, want the delivered-credential rejection wrapping the refresh failure", err)
	}

	expiry := time.Unix(1_700_000_000, 0)
	token := &fakeRefreshableToken{current: "first", next: "second", expiresAt: expiry}
	source, err := NewADORefreshingDeliveredCredentialSource(ADOCredentialKindBearer, "repo:push", token)
	if err != nil {
		t.Fatal(err)
	}
	got, err := source.Credential(context.Background())
	if err != nil || got.Kind != ADOCredentialKindBearer || got.Secret != "first" || !got.ExpiresAt.Equal(expiry) {
		t.Fatalf("Credential = %+v, %v", got, err)
	}
	source.(refreshableADOCredentialSource).Invalidate()
	if got, _ := source.Credential(context.Background()); got.Secret != "second" || token.invalidated != 1 {
		t.Fatalf("after Invalidate Credential = %+v (invalidated %d), want the re-resolved value", got, token.invalidated)
	}
	delivered := source.(deliveredADOCredential)
	if delivered.deliveredLabel() != "repo:push" || !delivered.deliveredExpiry().Equal(expiry) {
		t.Errorf("delivered label/expiry = %q/%v", delivered.deliveredLabel(), delivered.deliveredExpiry())
	}
}

func TestADOCredentialAuthorizationHeader(t *testing.T) {
	cases := map[string]struct {
		credential ADOCredential
		want       string
		wantErr    string
	}{
		"blank PAT":          {ADOCredential{Kind: ADOCredentialKindPAT, Secret: " "}, "", "ado PAT credential is empty"},
		"blank bearer":       {ADOCredential{Kind: ADOCredentialKindBearer}, "", "ado bearer credential is empty"},
		"unknown kind":       {ADOCredential{Kind: "cookie", Secret: "x"}, "", `unsupported ado credential kind "cookie"`},
		"PAT default user":   {ADOCredential{Kind: ADOCredentialKindPAT, Secret: "pat"}, "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:pat")), ""},
		"PAT named user":     {ADOCredential{Kind: ADOCredentialKindPAT, Secret: "pat", Username: "mona"}, "Basic " + base64.StdEncoding.EncodeToString([]byte("mona:pat")), ""},
		"bearer token value": {ADOCredential{Kind: ADOCredentialKindBearer, Secret: "jwt"}, "Bearer jwt", ""},
	}
	for name, tc := range cases {
		got, err := tc.credential.authorizationHeader()
		if tc.wantErr != "" {
			if err == nil || err.Error() != tc.wantErr || got != "" {
				t.Errorf("%s: header = %q, %v; want error %q", name, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: header = %q, %v; want %q", name, got, err, tc.want)
		}
	}
}

// fixedADOCredentialSource returns one credential or error.
type fixedADOCredentialSource struct {
	credential ADOCredential
	err        error
}

func (s fixedADOCredentialSource) Credential(context.Context) (ADOCredential, error) {
	return s.credential, s.err
}

func TestADOGitAuthEnvironmentRefusesUnusableInput(t *testing.T) {
	sourceErr := errors.New("az login expired")
	cases := map[string]struct {
		source  ADOCredentialSource
		remote  string
		wantErr string
		is      error
	}{
		"no source":        {nil, "https://dev.azure.com/org/p/_git/r", "ADO Git authentication requires a credential source", nil},
		"no remote":        {fixedADOCredentialSource{credential: ADOCredential{Kind: ADOCredentialKindPAT, Secret: "pat"}}, "  ", "ADO Git authentication requires a remote URL", nil},
		"credential fails": {fixedADOCredentialSource{err: sourceErr}, "https://dev.azure.com/org/p/_git/r", "resolve ADO Git credential: az login expired", sourceErr},
		"unusable kind":    {fixedADOCredentialSource{credential: ADOCredential{Kind: "cookie", Secret: "x"}}, "https://dev.azure.com/org/p/_git/r", `unsupported ado credential kind "cookie"`, nil},
		"empty bearer":     {fixedADOCredentialSource{credential: ADOCredential{Kind: ADOCredentialKindBearer}}, "https://dev.azure.com/org/p/_git/r", "ado bearer credential is empty", nil},
	}
	for name, tc := range cases {
		registrar := &spyGitRegistrar{}
		env, err := ADOGitAuthEnvironment(context.Background(), tc.source, registrar, tc.remote)
		if err == nil || err.Error() != tc.wantErr || (tc.is != nil && !errors.Is(err, tc.is)) || env != nil {
			t.Errorf("%s: env = %q, err = %v; want error %q", name, env, err, tc.wantErr)
		}
		if len(registrar.secrets) != 0 {
			t.Errorf("%s: registered %q for a refused environment", name, registrar.secrets)
		}
	}
}

// The child's Git environment drops any inherited GIT_CONFIG_* injection and
// prompt setting, so the only configuration it carries is the scoped header.
func TestADOGitAuthEnvironmentDropsInheritedGitConfig(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.sshCommand")
	t.Setenv("GIT_CONFIG_VALUE_0", "inherited-command")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	env, err := ADOGitAuthEnvironment(context.Background(), NewADOPATCredentialSource("", "pat"), nil, "https://dev.azure.com/org/p/_git/r")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		counts[name]++
		if strings.Contains(entry, "inherited-command") || strings.Contains(entry, "core.sshCommand") {
			t.Errorf("inherited git config survived: %q", entry)
		}
	}
	for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_TERMINAL_PROMPT"} {
		if counts[name] != 1 {
			t.Errorf("%s appears %d times, want exactly the one the provider sets", name, counts[name])
		}
	}
	if !slices.Contains(env, "GIT_CONFIG_COUNT=2") || !slices.Contains(env, "GIT_TERMINAL_PROMPT=0") || !slices.Contains(env, "GIT_CONFIG_KEY_0=credential.helper") {
		t.Errorf("env = %q, want the provider's own git configuration", env)
	}
}

// The cached bearer source never caches or returns an unusable token, and
// Invalidate forces the next call to fetch even while the cached token is
// still fresh.
func TestCachedADOBearerSourceRefusesUnusableTokensAndInvalidates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fetchErr := errors.New("identity endpoint down")
	cases := map[string]struct {
		token   adoBearerToken
		err     error
		wantErr string
	}{
		"fetch fails":     {err: fetchErr, wantErr: "identity endpoint down"},
		"empty token":     {token: adoBearerToken{token: " ", expiresAt: now.Add(time.Hour)}, wantErr: "ado bearer credential source returned an empty token"},
		"no expiry":       {token: adoBearerToken{token: "jwt"}, wantErr: "ado bearer credential source returned an invalid expiry"},
		"already expired": {token: adoBearerToken{token: "jwt", expiresAt: now.Add(-time.Minute)}, wantErr: "ado bearer credential source returned an invalid expiry"},
		"expires now":     {token: adoBearerToken{token: "jwt", expiresAt: now}, wantErr: "ado bearer credential source returned an invalid expiry"},
	}
	for name, tc := range cases {
		fetches := 0
		source := newCachedADOBearerSource(func() time.Time { return now }, func(context.Context) (adoBearerToken, error) {
			fetches++
			return tc.token, tc.err
		})
		for range 2 {
			got, err := source.Credential(context.Background())
			if err == nil || err.Error() != tc.wantErr || got.Secret != "" {
				t.Errorf("%s: Credential = %+v, %v; want %q", name, got, err, tc.wantErr)
			}
		}
		if fetches != 2 {
			t.Errorf("%s: fetched %d times, want a refused token never cached", name, fetches)
		}
	}

	fetches := 0
	source := newCachedADOBearerSource(func() time.Time { return now }, func(context.Context) (adoBearerToken, error) {
		fetches++
		return adoBearerToken{token: "jwt-" + strconv.Itoa(fetches), expiresAt: now.Add(time.Hour)}, nil
	})
	if _, err := source.Credential(cancelledContext()); !errors.Is(err, context.Canceled) || fetches != 0 {
		t.Fatalf("cancelled Credential = %v after %d fetches, want the cancellation and no fetch", err, fetches)
	}
	first, _ := source.Credential(context.Background())
	cached, _ := source.Credential(context.Background())
	source.Invalidate()
	fresh, _ := source.Credential(context.Background())
	if first.Secret != "jwt-1" || cached.Secret != "jwt-1" || fresh.Secret != "jwt-2" || fetches != 2 {
		t.Fatalf("tokens = %q, %q, %q after %d fetches; want the cached token until Invalidate", first.Secret, cached.Secret, fresh.Secret, fetches)
	}

	// A nil clock defaults to the wall clock rather than panicking.
	wall := newCachedADOBearerSource(nil, func(context.Context) (adoBearerToken, error) {
		return adoBearerToken{token: "jwt", expiresAt: time.Now().Add(time.Hour)}, nil
	})
	if got, err := wall.Credential(context.Background()); err != nil || got.Secret != "jwt" {
		t.Fatalf("Credential with the default clock = %+v, %v", got, err)
	}
}

func TestAzureIdentityCredentialSourceWrapsGetTokenFailure(t *testing.T) {
	cause := errors.New("no federated token")
	_, err := newAzureIdentityADOCredentialSource(&fakeAzureTokenCredential{err: cause}).Credential(context.Background())
	if !errors.Is(err, cause) || !strings.HasPrefix(err.Error(), "azure identity get Azure DevOps token: ") {
		t.Fatalf("Credential error = %v, want the wrapped GetToken failure", err)
	}
}

// The workload identity source needs a tenant, a client and a federated
// token file; clientID supplies the client when AZURE_CLIENT_ID does not.
// Construction reads no token, so these tests make no request.
func TestNewWorkloadIdentityADOCredentialSource(t *testing.T) {
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("federated"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AZURE_TENANT_ID", "00000000-0000-0000-0000-000000000001")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", tokenFile)
	// Unset, not empty: azidentity treats a set-but-empty variable as set.
	t.Setenv("AZURE_CLIENT_ID", "")
	if err := os.Unsetenv("AZURE_CLIENT_ID"); err != nil {
		t.Fatal(err)
	}

	if source, err := NewWorkloadIdentityADOCredentialSource(""); err == nil || source != nil ||
		!strings.HasPrefix(err.Error(), "create Azure workload identity credential: ") {
		t.Fatalf("without a client id: source = %v, err = %v; want the construction error", source, err)
	}
	source, err := NewWorkloadIdentityADOCredentialSource("00000000-0000-0000-0000-000000000002")
	if err != nil || source == nil {
		t.Fatalf("with a client id: source = %v, err = %v", source, err)
	}
}

// Construction reads no token, so these tests make no request. The
// environment pins which managed-identity host azidentity detects.
func TestNewManagedIdentityADOCredentialSource(t *testing.T) {
	for _, name := range []string{"IDENTITY_ENDPOINT", "IDENTITY_HEADER", "IDENTITY_SERVER_THUMBPRINT", "MSI_ENDPOINT", "MSI_SECRET", "IMDS_ENDPOINT"} {
		t.Setenv(name, "")
	}
	for _, clientID := range []string{"", "00000000-0000-0000-0000-000000000002"} {
		source, err := NewManagedIdentityADOCredentialSource(clientID)
		if err != nil || source == nil {
			t.Errorf("client %q: source = %v, err = %v", clientID, source, err)
		}
	}
	// Cloud Shell (MSI_ENDPOINT without a secret) has only a system identity,
	// so selecting a user-assigned client is refused at construction.
	t.Setenv("MSI_ENDPOINT", "http://127.0.0.1:1/token")
	source, err := NewManagedIdentityADOCredentialSource("00000000-0000-0000-0000-000000000002")
	if err == nil || source != nil || !strings.HasPrefix(err.Error(), "create Azure managed identity credential: ") {
		t.Fatalf("user-assigned client in Cloud Shell: source = %v, err = %v; want the construction error", source, err)
	}
}

func TestParseAzureCLIAccessTokenRefusesUnusableResponses(t *testing.T) {
	cases := map[string]struct {
		body    string
		want    time.Time
		wantErr string
	}{
		"not json":            {body: `not json`, wantErr: "decode Azure CLI access token response: "},
		"no access token":     {body: `{"accessToken":" ","expires_on":1700000000}`, wantErr: "azure CLI access token response is missing accessToken"},
		"no usable expiry":    {body: `{"accessToken":"jwt","expires_on":"soon","expiresOn":"later"}`, wantErr: "azure CLI access token response has an invalid expiry"},
		"no expiry at all":    {body: `{"accessToken":"jwt"}`, wantErr: "azure CLI access token response has an invalid expiry"},
		"unix expiry wins":    {body: `{"accessToken":"jwt","expires_on":1700000000,"expiresOn":"2030-01-01T00:00:00Z"}`, want: time.Unix(1_700_000_000, 0)},
		"falls back to local": {body: `{"accessToken":"jwt","expires_on":null,"expiresOn":"2023-11-14T22:13:20Z"}`, want: time.Unix(1_700_000_000, 0)},
	}
	for name, tc := range cases {
		got, err := parseAzureCLIAccessToken([]byte(tc.body))
		if tc.wantErr != "" {
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) || got.token != "" {
				t.Errorf("%s: token = %+v, %v; want error %q", name, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got.token != "jwt" || !got.expiresAt.Equal(tc.want) {
			t.Errorf("%s: token = %+v, %v; want expiry %v", name, got, err, tc.want)
		}
	}
}

func TestParseAzureCLIExpiryFormats(t *testing.T) {
	want := time.Unix(1_700_000_000, 0)
	for raw, wantErr := range map[string]string{
		``:                                    "expiry is empty",
		`null`:                                "expiry is empty",
		`""`:                                  "expiry is empty",
		`"tomorrow"`:                          "unrecognized expiry format",
		`1700000000`:                          "",
		`"1700000000"`:                        "",
		`"2023-11-14T22:13:20Z"`:              "",
		`"2023-11-14 22:13:20.000000 +00:00"`: "",
	} {
		got, err := parseAzureCLIExpiry([]byte(raw))
		if wantErr != "" {
			if err == nil || err.Error() != wantErr || !got.IsZero() {
				t.Errorf("%s: expiry = %v, %v; want %q", raw, got, err, wantErr)
			}
			continue
		}
		if err != nil || !got.Equal(want) {
			t.Errorf("%s: expiry = %v, %v; want %v", raw, got, err, want)
		}
	}
}
