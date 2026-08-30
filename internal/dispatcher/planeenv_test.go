package dispatcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/internal/telemetryclient"
)

// planeenv_test.go is Goobers#3897's evidence: the dispatcher stamps a
// COMPLETE, non-spoofable, least-privilege machine-plane environment into a
// goobers-CLI stage pod, or it stamps none of it.
//
// The failure it exists to prevent is silent. Every plane client selects its
// backend from os.Getenv inside the stage subprocess, so a stage pod missing
// GOOBERS_CLAIMS_ENDPOINT does not fail — it takes the FILE branch, against
// an empty scratch volume nothing reads, and reports success. A backlog claim
// that never reached the daemon's ledger looks exactly like one that did.

// cliAttempt is a goobers-CLI attempt with a complete set of plane bearers,
// the shape Dispatch produces.
func cliAttempt() Attempt {
	attempt := testAttempt()
	attempt.CLIStage = true
	attempt.Command = []string{"goobers", "backlog-query", "--claim"}
	attempt.PlaneTokens = PlaneTokens{
		Claims:    "goobers-pod.claims",
		State:     "goobers-pod.state",
		Journal:   "goobers-pod.journal",
		Telemetry: "goobers-pod.telemetry",
	}
	return attempt
}

// podEnvMap reads a rendered pod's stage-container environment as a map,
// keeping the LAST value for a duplicated name — which is what the kubelet
// hands the container, and therefore the only reading that can prove an
// override was refused rather than merely appended after.
func podEnvMap(pod *corev1.Pod) map[string]string {
	got := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		got[e.Name] = e.Value
	}
	return got
}

// Every plane client's Select contract, in one place. If a client renames a
// variable or adds a required one, this table is what fails — not a stage pod
// six weeks later.
func planeContract() []struct {
	plane          string
	endpoint       string
	token          string
	tokenValue     func(PlaneTokens) string
	alsoRequiresID bool
	alsoRequiresGa bool
} {
	return []struct {
		plane          string
		endpoint       string
		token          string
		tokenValue     func(PlaneTokens) string
		alsoRequiresID bool
		alsoRequiresGa bool
	}{
		{"claims", claimsclient.EnvEndpoint, claimsclient.EnvToken, func(t PlaneTokens) string { return t.Claims }, true, false},
		{"state", stateclient.EnvEndpoint, stateclient.EnvToken, func(t PlaneTokens) string { return t.State }, false, true},
		{"journal", journalclient.EnvEndpoint, journalclient.EnvToken, func(t PlaneTokens) string { return t.Journal }, true, false},
		{"telemetry", telemetryclient.EnvEndpoint, telemetryclient.EnvToken, func(t PlaneTokens) string { return t.Telemetry }, false, true},
	}
}

// The names this package restates must BE the names the clients select on.
// podspec.go deliberately does not import the client packages (that would
// invert the dependency — a client is a stage-side consumer, the dispatcher
// is the daemon-side producer), so nothing but this test stops the two
// spellings from drifting apart, and drift is silent in exactly the way the
// whole issue is about.
func TestPlaneEnvNamesMatchTheClients(t *testing.T) {
	for _, want := range []struct{ dispatcher, client, who string }{
		{ClaimsEndpointEnv, claimsclient.EnvEndpoint, "claimsclient endpoint"},
		{ClaimsTokenEnv, claimsclient.EnvToken, "claimsclient bearer"},
		{StateEndpointEnv, stateclient.EnvEndpoint, "stateclient endpoint"},
		{StateTokenEnv, stateclient.EnvToken, "stateclient bearer"},
		{JournalEndpointEnv, journalclient.EnvEndpoint, "journalclient endpoint"},
		{JournalTokenEnv, journalclient.EnvToken, "journalclient bearer"},
		{TelemetryEndpointEnv, telemetryclient.EnvEndpoint, "telemetryclient endpoint"},
		{TelemetryTokenEnv, telemetryclient.EnvToken, "telemetryclient bearer"},
	} {
		if want.dispatcher != want.client {
			t.Errorf("%s: dispatcher stamps %q, the client selects on %q", want.who, want.dispatcher, want.client)
		}
	}
	// And the run identity the clients ALSO require: claims and journal
	// contain a caller to its run, state and telemetry are gaggle-scoped.
	for _, want := range []struct{ dispatcher, client, who string }{
		{EnvRunID, claimsclient.EnvRunID, "claimsclient run identity"},
		{EnvRunID, journalclient.EnvRunID, "journalclient run identity"},
		{EnvGaggle, stateclient.EnvGaggle, "stateclient gaggle"},
		{EnvGaggle, telemetryclient.EnvGaggle, "telemetryclient gaggle"},
	} {
		if want.dispatcher != want.client {
			t.Errorf("%s: dispatcher stamps %q, the client selects on %q", want.who, want.dispatcher, want.client)
		}
	}
}

// The scope vocabulary this package restates must be podauth's.
func TestPlaneScopeNamesMatchPodauth(t *testing.T) {
	for _, want := range []struct{ here, there string }{
		{scopeClaims, podauth.ScopeClaims},
		{scopeState, podauth.ScopeState},
		{scopeJournal, podauth.ScopeJournal},
		{scopeTelemetry, podauth.ScopeTelemetry},
	} {
		if want.here != want.there {
			t.Errorf("dispatcher mints scope %q, podauth knows it as %q", want.here, want.there)
		}
	}
	// The one the dispatcher must NEVER mint for a stage: surrender is the
	// pod token's authority, and handing it to a stage subprocess would let
	// workflow-authored content author its own result.
	for _, minted := range []string{scopeClaims, scopeState, scopeJournal, scopeTelemetry} {
		if minted == podauth.ScopeSurrender {
			t.Fatalf("scope %q is the surrender scope; a stage bearer must never carry it", minted)
		}
	}
}

// The headline: a rendered goobers-CLI stage pod carries the COMPLETE set.
func TestRenderedCLIStagePodCarriesEveryPlanePair(t *testing.T) {
	cfg := testConfig()
	attempt := cliAttempt()
	pod, err := RenderPod(cfg, attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	env := podEnvMap(pod)

	for _, plane := range planeContract() {
		if got := env[plane.endpoint]; got != cfg.WriteAPIBase {
			t.Errorf("%s plane endpoint %s = %q, want the daemon write API %q", plane.plane, plane.endpoint, got, cfg.WriteAPIBase)
		}
		if got, want := env[plane.token], plane.tokenValue(attempt.PlaneTokens); got != want {
			t.Errorf("%s plane bearer %s = %q, want %q", plane.plane, plane.token, got, want)
		}
		if plane.alsoRequiresID && env[EnvRunID] != attempt.RunID {
			t.Errorf("%s plane also selects on %s, which is %q", plane.plane, EnvRunID, env[EnvRunID])
		}
		if plane.alsoRequiresGa && env[EnvGaggle] != attempt.Gaggle {
			t.Errorf("%s plane also selects on %s, which is %q", plane.plane, EnvGaggle, env[EnvGaggle])
		}
	}
}

// Least privilege, asserted as a property of the rendered spec rather than of
// the minter: the four bearers are four different values, and none of them is
// GOOBERS_POD_TOKEN. Reusing the pod token would be the easy implementation
// and the wrong one — it authorizes the surrender route, so a stage
// subprocess holding it could declare its own stage successful.
func TestPlaneBearersAreDistinctAndNeverThePodToken(t *testing.T) {
	pod, err := RenderPod(testConfig(), cliAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	env := podEnvMap(pod)

	seen := map[string]string{}
	for _, name := range []string{ClaimsTokenEnv, StateTokenEnv, JournalTokenEnv, TelemetryTokenEnv, EnvPodToken} {
		value := env[name]
		if value == "" {
			t.Fatalf("%s is empty in a rendered CLI stage pod", name)
		}
		if prior, dup := seen[value]; dup {
			t.Errorf("%s and %s carry the SAME bearer; each plane must hold its own least-privilege token", prior, name)
		}
		seen[value] = name
	}
}

// ALL OR NOTHING. Each of these attempts is missing exactly one ingredient of
// a complete plane environment, and each must produce ZERO plane variables —
// not the seven it could have stamped. A partial set does not degrade to the
// file backend: claimsclient.Select refuses an endpoint without a bearer, so
// a half-stamped pod fails INSIDE the pod, after scheduling, on the far side
// of the boundary this whole change exists to make legible.
func TestPartialPlaneConfigStampsNothing(t *testing.T) {
	complete := cliAttempt()
	for _, tc := range []struct {
		name    string
		mutate  func(*Config, *Attempt)
		because string
	}{
		{"not a CLI stage", func(_ *Config, a *Attempt) { a.CLIStage = false },
			"an ordinary stage is stripped of the whole control plane in the pod; stamping four bearers into a spec anyone with get-pod can read buys nothing"},
		{"no write API base", func(c *Config, _ *Attempt) { c.WriteAPIBase = "" },
			"the loopback/self-host posture, where the stage's file backends are the correct answer"},
		{"no run identity", func(_ *Config, a *Attempt) { a.RunID = "" },
			"the claims and journal planes contain every call to the caller's own run"},
		{"no gaggle", func(_ *Config, a *Attempt) { a.Gaggle = "" },
			"the scheduler-state and telemetry planes are gaggle-scoped"},
		{"one bearer missing", func(_ *Config, a *Attempt) { a.PlaneTokens.Journal = "" },
			"three planes working and one refusing is the worst of both"},
		{"no bearers at all", func(_ *Config, a *Attempt) { a.PlaneTokens = PlaneTokens{} },
			"no minter was configured"},
		{"a repeated bearer", func(_ *Config, a *Attempt) { a.PlaneTokens.State = a.PlaneTokens.Claims },
			"a minter that repeats itself has not confined anything"},
		{"a bearer equal to the pod token", func(_ *Config, a *Attempt) { a.PlaneTokens.Claims = a.PodToken },
			"the pod token authorizes surrender; no plane bearer may equal it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, attempt := testConfig(), complete
			tc.mutate(&cfg, &attempt)
			pod, err := RenderPod(cfg, attempt, linuxRunner())
			if err != nil {
				t.Fatalf("RenderPod: %v", err)
			}
			env := podEnvMap(pod)
			for _, name := range DispatcherPlaneEnv {
				if value, present := env[name]; present {
					t.Errorf("%s = %q was stamped despite an incomplete plane environment (%s); the rule is all eight or none", name, value, tc.because)
				}
			}
		})
	}
}

// SPOOF REFUSAL, by name. stageEnv stamps the dispatcher's block FIRST and a
// stage's declared env AFTER, and the kubelet resolves a duplicated name
// last-wins — so without this refusal a workflow could point the
// scheduler-state plane at a server it chose, with the daemon's own bearer
// attached, by writing four lines of YAML.
func TestWorkflowEnvCannotOverrideAControlVariable(t *testing.T) {
	for _, name := range DispatcherControlEnv {
		t.Run(name, func(t *testing.T) {
			attempt := cliAttempt()
			attempt.Env = map[string]string{name: "http://attacker.example"}
			_, err := RenderPod(testConfig(), attempt, linuxRunner())
			var override *ControlEnvOverrideError
			if !errors.As(err, &override) {
				t.Fatalf("RenderPod err = %v, want a ControlEnvOverrideError for %s", err, name)
			}
			if override.Key != name {
				t.Fatalf("refused key = %q, want %q", override.Key, name)
			}
		})
	}
}

// SPOOF REFUSAL, by value. `env: {X: "$(GOOBERS_CLAIMS_TOKEN)"}` copies the
// bearer into a name the stage keeps — the kubelet expands it against the
// variables declared earlier in the same container, which is precisely where
// the control plane sits.
func TestWorkflowEnvCannotDereferenceAControlVariable(t *testing.T) {
	for _, name := range []string{EnvPodToken, ClaimsTokenEnv, StateTokenEnv, JournalTokenEnv, TelemetryTokenEnv, EnvDaemonAPI} {
		t.Run(name, func(t *testing.T) {
			attempt := cliAttempt()
			attempt.Env = map[string]string{"EXFIL": "prefix-$(" + name + ")-suffix"}
			_, err := RenderPod(testConfig(), attempt, linuxRunner())
			var override *ControlEnvOverrideError
			if !errors.As(err, &override) {
				t.Fatalf("RenderPod err = %v, want a ControlEnvOverrideError dereferencing %s", err, name)
			}
			if override.Dereferences != name {
				t.Fatalf("refused dereference = %q, want %q", override.Dereferences, name)
			}
		})
	}
	// An ordinary $(VAR) expansion of a stage's OWN variable is untouched:
	// the refusal is narrow on purpose.
	attempt := cliAttempt()
	attempt.Env = map[string]string{"BASE": "/opt", "FULL": "$(BASE)/bin"}
	if _, err := RenderPod(testConfig(), attempt, linuxRunner()); err != nil {
		t.Fatalf("a stage composing its own variables must still render: %v", err)
	}
}

// Inputs cannot collide by construction — InputEnvVar prefixes every one with
// GOOBERS_INPUT_ — but "by construction" is a property of a function someone
// could change, so it is asserted rather than assumed.
func TestInputEnvVarCannotCollideWithControlEnv(t *testing.T) {
	for _, name := range DispatcherControlEnv {
		// The adversarial input name: whatever suffix would produce the
		// control variable, if the prefix were ever dropped.
		for _, probe := range []string{name, strings.TrimPrefix(name, "GOOBERS_"), strings.ToLower(name)} {
			if got := InputEnvVar(probe); slices.Contains(DispatcherControlEnv, got) {
				t.Fatalf("InputEnvVar(%q) = %q, which is the control variable %q", probe, got, name)
			}
		}
	}
	attempt := cliAttempt()
	attempt.Inputs = map[string]string{"claims-endpoint": "http://attacker.example"}
	pod, err := RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if got := podEnvMap(pod)[ClaimsEndpointEnv]; got != testConfig().WriteAPIBase {
		t.Fatalf("%s = %q after a hostile input name; inputs must not reach the control plane", ClaimsEndpointEnv, got)
	}
}

// The operator-declared passthrough list carries NAMES into the
// env:default-deny allowlist, never values — and the in-pod control strip
// runs after the allowlist rebuild. So naming a control variable in the
// passthrough cannot re-admit it to a non-CLI stage, and cannot change its
// value for any stage.
func TestEnvPassthroughCannotSpoofTheControlPlane(t *testing.T) {
	cfg := testConfig()
	cfg.EnvPassthrough = append([]string{"OPERATOR_VAR"}, DispatcherControlEnv...)
	pod, err := RenderPod(cfg, cliAttempt(), envDenyRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	env := podEnvMap(pod)
	for _, plane := range planeContract() {
		if got := env[plane.endpoint]; got != cfg.WriteAPIBase {
			t.Errorf("%s = %q; the passthrough list must not be able to change a stamped endpoint", plane.endpoint, got)
		}
	}
	if env[EnvPodToken] != cliAttempt().PodToken {
		t.Errorf("%s = %q; the passthrough list must not be able to change the pod token", EnvPodToken, env[EnvPodToken])
	}
}

// A CLI stage on a class enforcing env:default-deny must keep its plane
// variables. The rebuild from GOOBERS_STAGE_ENV_ALLOW runs BEFORE the
// CLI/non-CLI control strip, so a plane name absent from the allowlist is a
// plane variable the stage loses — and losing it means the file branch, on a
// scratch volume, silently.
func TestEnvDefaultDenyAllowlistCarriesThePlaneEnv(t *testing.T) {
	pod, err := RenderPod(testConfig(), cliAttempt(), envDenyRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	var allow []string
	if err := json.Unmarshal([]byte(podEnvMap(pod)[EnvStageEnvAllow]), &allow); err != nil {
		t.Fatalf("decode %s: %v", EnvStageEnvAllow, err)
	}
	for _, name := range DispatcherPlaneEnv {
		if !slices.Contains(allow, name) {
			t.Errorf("%s is not in the env:default-deny allowlist; a CLI stage on this class would lose it and take the local-file branch", name)
		}
	}
	// And the pod token still is NOT, because the control strip removes it
	// again immediately afterwards — the allowlist may not be a way in.
	if slices.Contains(allow, EnvPodToken) {
		t.Errorf("%s must never be allowlisted", EnvPodToken)
	}
}

// --- Dispatch-level minting ------------------------------------------------

// recordingMinter is a ScopedTokenMinter that reports what it was asked for.
type recordingMinter struct {
	scopes []string
	ttls   []time.Duration
	n      int
}

func (m *recordingMinter) Mint(runID string, ttl time.Duration) (string, error) {
	m.n++
	return fmt.Sprintf("goobers-pod.unscoped-%d", m.n), nil
}

func (m *recordingMinter) MintScoped(runID string, ttl time.Duration, scopes ...string) (string, error) {
	m.n++
	m.scopes = append(m.scopes, strings.Join(scopes, ","))
	m.ttls = append(m.ttls, ttl)
	return fmt.Sprintf("goobers-pod.%s-%d", strings.Join(scopes, "-"), m.n), nil
}

// unscopedMinter satisfies only the older seam.
type unscopedMinter struct{}

func (unscopedMinter) Mint(runID string, ttl time.Duration) (string, error) {
	return "goobers-pod.unscoped", nil
}

// Dispatch mints ONE bearer per plane, each carrying exactly its own scope.
func TestMintPlaneTokensRequestsOneScopePerPlane(t *testing.T) {
	minter := &recordingMinter{}
	d := &Dispatcher{cfg: Config{WriteAPIBase: "http://daemon:7777", TokenMinter: minter}}
	attempt := testAttempt()
	attempt.CLIStage = true
	attempt.PlaneTokens = PlaneTokens{}

	if err := d.mintPlaneTokens(&attempt); err != nil {
		t.Fatalf("mintPlaneTokens: %v", err)
	}
	want := []string{scopeClaims, scopeState, scopeJournal, scopeTelemetry}
	if !slices.Equal(minter.scopes, want) {
		t.Fatalf("minted scopes = %v, want exactly %v — one plane each, never a union", minter.scopes, want)
	}
	for i, ttl := range minter.ttls {
		if ttl != PlaneTokenTTL {
			t.Errorf("bearer %d minted with ttl %s, want the bounded %s", i, ttl, PlaneTokenTTL)
		}
	}
	if !attempt.PlaneTokens.Complete() {
		t.Fatalf("plane tokens = %+v, want all four", attempt.PlaneTokens)
	}
	if !attempt.PlaneTokens.Distinct(attempt.PodToken) {
		t.Fatalf("plane tokens = %+v are not distinct from the pod token %q", attempt.PlaneTokens, attempt.PodToken)
	}
}

// A non-CLI stage gets none: nothing in the pod would read them, and a spec
// anyone with get-pod can read should not carry credentials nobody uses.
func TestMintPlaneTokensSkipsNonCLIStages(t *testing.T) {
	minter := &recordingMinter{}
	d := &Dispatcher{cfg: Config{WriteAPIBase: "http://daemon:7777", TokenMinter: minter}}
	attempt := testAttempt()
	attempt.CLIStage = false
	if err := d.mintPlaneTokens(&attempt); err != nil {
		t.Fatalf("mintPlaneTokens: %v", err)
	}
	if !attempt.PlaneTokens.Empty() {
		t.Fatalf("plane tokens = %+v for a non-CLI stage, want none", attempt.PlaneTokens)
	}
	if minter.n != 0 {
		t.Fatalf("minter called %d times for a non-CLI stage", minter.n)
	}
}

// A minter that cannot confine a bearer to one plane is REFUSED, loudly. The
// alternatives are both worse: reusing the pod token hands a stage the
// authority to surrender its own result, and stamping nothing would put a
// pod-executed claiming stage back on the silent local-file branch.
func TestMintPlaneTokensRefusesAnUnscopedMinter(t *testing.T) {
	d := &Dispatcher{cfg: Config{WriteAPIBase: "http://daemon:7777", TokenMinter: unscopedMinter{}}}
	attempt := testAttempt()
	attempt.CLIStage = true
	err := d.mintPlaneTokens(&attempt)
	if err == nil {
		t.Fatal("mintPlaneTokens accepted a minter that cannot scope a bearer")
	}
	if !strings.Contains(err.Error(), "GOOBERS_POD_TOKEN") {
		t.Fatalf("refusal = %v, want it to say why reusing the pod token is not the fallback", err)
	}
}

// Missing run or gaggle identity is refused for the same reason: the planes
// are contained by them, so a stage without them cannot be given a working
// plane environment, and a partial one is not an option.
func TestMintPlaneTokensRefusesIncompleteIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Attempt)
	}{
		{"no run id", func(a *Attempt) { a.RunID = "" }},
		{"no gaggle", func(a *Attempt) { a.Gaggle = "" }},
		{"blank gaggle", func(a *Attempt) { a.Gaggle = "   " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Dispatcher{cfg: Config{WriteAPIBase: "http://daemon:7777", TokenMinter: &recordingMinter{}}}
			attempt := testAttempt()
			attempt.CLIStage = true
			tc.mutate(&attempt)
			if err := d.mintPlaneTokens(&attempt); err == nil {
				t.Fatal("mintPlaneTokens accepted an attempt it cannot give a complete plane environment")
			}
		})
	}
}

// The loopback/self-host posture: no write API base means no planes, and that
// is not an error — it is the deployment where the file backends are correct.
func TestMintPlaneTokensIsSilentWithoutAWriteAPI(t *testing.T) {
	minter := &recordingMinter{}
	d := &Dispatcher{cfg: Config{TokenMinter: minter}}
	attempt := testAttempt()
	attempt.CLIStage = true
	if err := d.mintPlaneTokens(&attempt); err != nil {
		t.Fatalf("mintPlaneTokens: %v", err)
	}
	if !attempt.PlaneTokens.Empty() || minter.n != 0 {
		t.Fatalf("plane tokens = %+v (minted %d) with no write API base", attempt.PlaneTokens, minter.n)
	}
}

// podauth's real minters must satisfy the scoped seam — the wiring in
// cmd/goobers hands a *podauth.SignedKey through dispatcher.TokenMinter, and
// a type assertion that quietly fails would disable plane stamping across the
// whole deployment without a single error.
func TestPodauthMintersSatisfyTheScopedSeam(t *testing.T) {
	registry := podauth.NewRegistry()
	if _, ok := any(registry).(ScopedTokenMinter); !ok {
		t.Error("*podauth.Registry no longer satisfies ScopedTokenMinter")
	}
	signed, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatalf("NewSignedKey: %v", err)
	}
	if _, ok := any(signed).(ScopedTokenMinter); !ok {
		t.Error("*podauth.SignedKey no longer satisfies ScopedTokenMinter")
	}
	// And the tokens a real minter produces satisfy the distinctness the
	// dispatcher asserts — the property is checked at dispatch precisely
	// because it depends on a minter this package did not write.
	d := &Dispatcher{cfg: Config{WriteAPIBase: "http://daemon:7777", TokenMinter: signed}}
	attempt := testAttempt()
	attempt.CLIStage = true
	if err := d.mintPlaneTokens(&attempt); err != nil {
		t.Fatalf("mintPlaneTokens with the real signed minter: %v", err)
	}
	if !attempt.PlaneTokens.Distinct(attempt.PodToken) {
		t.Fatalf("the signed minter produced repeated bearers: %+v", attempt.PlaneTokens)
	}
}
