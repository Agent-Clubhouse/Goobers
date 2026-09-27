package credreadiness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/credentials"
)

var probeNow = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func envRef() credentials.TokenRef  { return credentials.TokenRef{Name: "r", Env: "GOOBERS_TEST_PAT"} }
func fileRef() credentials.TokenRef { return credentials.TokenRef{Name: "r", File: "/etc/goobers/pat"} }
func storeRef() credentials.TokenRef {
	return credentials.TokenRef{Name: "r", Store: "vault/ado-pat"}
}
func cliRef() credentials.TokenRef {
	return credentials.TokenRef{Name: "r", GitHubCLI: &credentials.GitHubCLIRef{Hostname: "github.com", User: "octocat"}}
}

func resolves(value string, expiresAt time.Time) ExpiringResolve {
	return func(context.Context) (string, time.Time, error) { return value, expiresAt, nil }
}

func fails(err error) ExpiringResolve {
	return func(context.Context) (string, time.Time, error) { return "", time.Time{}, err }
}

// Every supported source kind is described by kind and by a value-free source
// name. An unrecognized ref must NOT be described as env: that fallback is how
// an unchecked credential acquires a clean readiness result (#4816).
func TestDescribeCoversEverySourceKindAndFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ref        credentials.TokenRef
		wantKind   SourceKind
		wantSource string
	}{
		{"env", envRef(), SourceEnv, "GOOBERS_TEST_PAT"},
		{"file", fileRef(), SourceFile, "/etc/goobers/pat"},
		{"keychain", credentials.TokenRef{Name: "r", Keychain: "goobers-ado"}, SourceKeychain, "goobers-ado"},
		{"store", storeRef(), SourceStore, "vault/ado-pat"},
		{"github cli", cliRef(), SourceGitHubCLI, "github.com/octocat"},
		{"unset", credentials.TokenRef{Name: "r"}, SourceUnsupported, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, source := Describe(tc.ref)
			if kind != tc.wantKind || source != tc.wantSource {
				t.Fatalf("Describe = (%q, %q), want (%q, %q)", kind, source, tc.wantKind, tc.wantSource)
			}
		})
	}
}

// An unsupported ref must fail closed with its own stable code and must not be
// probed at all -- resolving it would be the env fallback in another shape.
func TestProbeUnsupportedSourceFailsClosedWithoutResolving(t *testing.T) {
	resolved := false
	resolve := func(context.Context) (string, time.Time, error) {
		resolved = true
		return "secret", time.Time{}, nil
	}
	check := Probe(context.Background(), "agent:model", credentials.TokenRef{Name: "r"}, resolve, nil, probeNow)
	if check.Status != StatusUnsupportedSource {
		t.Fatalf("status = %q, want %q", check.Status, StatusUnsupportedSource)
	}
	if resolved {
		t.Fatal("an unsupported source was resolved anyway")
	}
	if check.Status.Asserted() {
		t.Fatal("unsupported source asserted readiness")
	}
}

// The four states #5261 names, across the four source kinds it names.
func TestProbeClassifiesEveryStateAcrossSourceKinds(t *testing.T) {
	future := probeNow.Add(time.Hour)
	past := probeNow.Add(-time.Hour)

	for _, source := range []struct {
		name string
		ref  credentials.TokenRef
	}{
		{"env", envRef()},
		{"file", fileRef()},
		{"store", storeRef()},
		{"github cli", cliRef()},
	} {
		for _, state := range []struct {
			name    string
			resolve ExpiringResolve
			want    Status
		}{
			{"usable", resolves("tok", future), StatusUsable},
			{"expired", resolves("tok", past), StatusExpired},
			{"refresh failed", fails(fmt.Errorf("interactive sign-in required: %w", ErrRefreshFailed)), StatusRefreshFailed},
			{"unobservable", fails(fmt.Errorf("service account cannot read it: %w", ErrUnobservable)), StatusUnobservable},
			{"empty", resolves("   ", future), StatusEmpty},
		} {
			t.Run(source.name+"/"+state.name, func(t *testing.T) {
				check := Probe(context.Background(), "agent:model", source.ref, state.resolve, nil, probeNow)
				if check.Status != state.want {
					t.Fatalf("status = %q, want %q (detail %q)", check.Status, state.want, check.Detail)
				}
				if got, _ := Describe(source.ref); check.Kind != got {
					t.Fatalf("kind = %q, want %q", check.Kind, got)
				}
			})
		}
	}
}

// An expiry the source did not state must stay explicitly unknown rather than
// being reported as unbounded validity.
func TestProbeUnknownExpiryStaysExplicit(t *testing.T) {
	check := Probe(context.Background(), "agent:model", envRef(), resolves("tok", time.Time{}), nil, probeNow)
	if check.Status != StatusUsable {
		t.Fatalf("status = %q, want usable", check.Status)
	}
	if check.ExpiryKnown {
		t.Fatal("an unstated expiry was reported as known")
	}
	if !strings.Contains(check.Detail, "unknown") {
		t.Fatalf("unstated expiry is not called out: %q", check.Detail)
	}
}

// A check that ran out of time established nothing. Reporting it as a failure
// would be as wrong as reporting it as a pass.
func TestProbeTimeoutIsUnobservableNotFailed(t *testing.T) {
	check := Probe(context.Background(), "agent:model", storeRef(), fails(context.DeadlineExceeded), nil, probeNow)
	if check.Status != StatusUnobservable {
		t.Fatalf("status = %q, want %q", check.Status, StatusUnobservable)
	}
}

type fixedScrubber struct{ secret string }

func (s fixedScrubber) Scrub(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte(s.secret), []byte("REDACTED"))
}

// A resolver error can quote the value it failed on, so every detail is
// scrubbed -- and the value never reaches the Check by any other route.
func TestProbeScrubsDetailAndNeverCarriesTheValue(t *testing.T) {
	const secret = "ghp_supersecrettoken"
	check := Probe(context.Background(), "agent:model", envRef(),
		fails(errors.New("rejected token "+secret)), fixedScrubber{secret: secret}, probeNow)
	if strings.Contains(check.Detail, secret) {
		t.Fatalf("detail leaked the credential: %q", check.Detail)
	}
	if !strings.Contains(check.Detail, "REDACTED") {
		t.Fatalf("detail was not scrubbed: %q", check.Detail)
	}

	usable := Probe(context.Background(), "agent:model", envRef(), resolves(secret, probeNow.Add(time.Hour)), nil, probeNow)
	if strings.Contains(usable.Summarize()+usable.Detail+usable.Source, secret) {
		t.Fatal("a successful check carried the credential value")
	}
}

// The acceptance criterion that motivates the whole package: a readiness result
// asserted by an interactive user must not stand in for one under the service
// identity that actually runs the work.
func TestReportDoesNotTransferBetweenIdentities(t *testing.T) {
	interactive := Observer{Account: "mason", Machine: "build-01"}
	service := Observer{Account: "LocalSystem", Machine: "build-01"}
	otherMachine := Observer{Account: "mason", Machine: "build-02"}

	report := Report{
		Observer:  interactive,
		CheckedAt: probeNow,
		Checks:    []Check{{Name: "agent:model", Status: StatusUsable}},
	}
	if !report.Ready() {
		t.Fatal("a report of usable checks is not ready")
	}
	if !report.AssertsFor(interactive) {
		t.Fatal("report does not assert for the identity that made it")
	}
	if report.AssertsFor(service) {
		t.Fatal("an interactive readiness result transferred to the service identity")
	}
	if report.AssertsFor(otherMachine) {
		t.Fatal("a readiness result transferred to another machine")
	}
}

// An unobserved identity anchors nothing -- including another unobserved one,
// since two unknowns are not known to be the same identity.
func TestReportWithUnknownObserverAssertsNothing(t *testing.T) {
	unknown := Observer{Account: unknownIdentity, Machine: unknownIdentity}
	report := Report{Observer: unknown, Checks: []Check{{Name: "agent:model", Status: StatusUsable}}}
	if report.AssertsFor(unknown) {
		t.Fatal("two unknown identities were treated as the same identity")
	}
	if report.AssertsFor(Observer{Account: "mason", Machine: "build-01"}) {
		t.Fatal("a report from an unknown identity asserted for a known one")
	}
	if (Observer{Account: "mason", Machine: ""}).Known() {
		t.Fatal("a half-observed identity reported itself as known")
	}
}

// An unobservable check is the absence of evidence and must not round up to a
// pass, and an empty report asserts nothing.
func TestReportReadyRequiresPositiveEvidence(t *testing.T) {
	observer := Observer{Account: "mason", Machine: "build-01"}
	unobservable := Report{Observer: observer, Checks: []Check{
		{Name: "agent:model", Status: StatusUsable},
		{Name: "ado-repository", Status: StatusUnobservable},
	}}
	if unobservable.Ready() {
		t.Fatal("an unobservable check was rounded up to ready")
	}
	if got := unobservable.Unready(); len(got) != 1 || got[0].Name != "ado-repository" {
		t.Fatalf("Unready = %+v, want the unobservable check", got)
	}
	if (Report{Observer: observer}).Ready() {
		t.Fatal("a report with no checks claimed readiness")
	}
}

// The three observations must stay distinguishable in the report.
func TestCategoriesSeparateTheThreeObservations(t *testing.T) {
	categories := map[Category]bool{}
	for _, c := range []Category{CategoryAuthentication, CategoryTransport, CategoryToolAuthorization} {
		if c == "" {
			t.Fatal("a category has no stable code")
		}
		categories[c] = true
	}
	if len(categories) != 3 {
		t.Fatalf("categories collapsed: %v", categories)
	}
	// A probe reports on authentication only; transport and tool
	// authorization are separate observations and must not be implied by it.
	check := Probe(context.Background(), "agent:model", envRef(), resolves("tok", probeNow.Add(time.Hour)), nil, probeNow)
	if check.Category != CategoryAuthentication {
		t.Fatalf("credential probe claimed category %q", check.Category)
	}
}

// User-scoped sources are flagged, because an operator reading one in their own
// session is not evidence that a service account can.
func TestUserScopedSourcesAreFlagged(t *testing.T) {
	for _, kind := range []SourceKind{SourceKeychain, SourceGitHubCLI} {
		if !kind.UserScoped() {
			t.Fatalf("%q is not flagged as user-scoped", kind)
		}
	}
	for _, kind := range []SourceKind{SourceEnv, SourceFile, SourceStore, SourceGitHubApp} {
		if kind.UserScoped() {
			t.Fatalf("%q was flagged as user-scoped", kind)
		}
	}
}

func TestProbeSourceReportsGitHubAppExpiry(t *testing.T) {
	expiresAt := probeNow.Add(time.Hour)
	check := ProbeSource(context.Background(), "agent:model", SourceGitHubApp, "acme/web",
		func(context.Context) (string, time.Time, error) {
			return "ghs_minted", expiresAt, nil
		}, nil, probeNow)
	if check.Status != StatusUsable || check.Kind != SourceGitHubApp || check.Source != "acme/web" {
		t.Fatalf("check = %+v, want usable github_app source", check)
	}
	if !check.ExpiryKnown || !check.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("expiry = %v known=%v, want %v", check.ExpiresAt, check.ExpiryKnown, expiresAt)
	}
}

// A ref with no resolver wired cannot be checked; that is an observation, not
// an absence of one.
func TestProbeWithoutResolverIsUnobservable(t *testing.T) {
	check := Probe(context.Background(), "agent:model", envRef(), nil, nil, probeNow)
	if check.Status != StatusUnobservable {
		t.Fatalf("status = %q, want %q", check.Status, StatusUnobservable)
	}
}

func TestCurrentObserverDegradesExplicitly(t *testing.T) {
	observer := CurrentObserver()
	if observer.Account == "" || observer.Machine == "" {
		t.Fatalf("CurrentObserver left a field empty rather than explicit: %+v", observer)
	}
}
