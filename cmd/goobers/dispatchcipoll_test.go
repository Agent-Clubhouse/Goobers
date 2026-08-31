package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// dispatchcipoll_test.go is decision 005 C5's evidence (#3881): `kind:
// ci-poll` runs IN A POD, and what it produces there is what the same stage
// produces on a self runner.
//
// THE SHAPE OF THE PARITY ASSERTIONS. Every parity case drives BOTH substrates
// from ONE fixture poller and ONE set of declared inputs, then compares the
// projected envelopes:
//
//   - the LOCAL side is internal/executor's ciPollKindExecutor — the exact
//     in-process kind executor the runner registers (runnerwiring_executors.go
//     wraps it with credential resolution and a quota observer; the polling
//     behaviour under test is this);
//   - the POD side is runCIPollStage, driven through the environment a real
//     stage pod carries and a real credential plane (httptest) it resolves
//     provider:pr:write against.
//
// Sharing the fixture is the point. Two fakes would only prove that two fakes
// differ, so both sides resolve their poller through the SAME newPRPoller seam
// — which is also why production shares it.

// fakeCIPoller answers each poll from a scripted queue, so a case can script
// "pending, pending, passing" or "rate limited forever" without a clock.
type fakeCIPoller struct {
	results []providers.PullRequestPollResult
	errs    []error
	calls   int
}

func (p *fakeCIPoller) PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	i := p.calls
	p.calls++
	if i < len(p.errs) && p.errs[i] != nil {
		return providers.PullRequestPollResult{}, p.errs[i]
	}
	if i < len(p.results) {
		return p.results[i], nil
	}
	if len(p.errs) > 0 {
		return providers.PullRequestPollResult{}, p.errs[len(p.errs)-1]
	}
	return p.results[len(p.results)-1], nil
}

// ciPollFixture is one stage declaration, expressed once and rendered for
// each substrate: as an InvocationEnvelope for the in-process executor, and as
// pod environment variables for dispatch-exec.
type ciPollFixture struct {
	inputs       map[string]string
	capabilities []string
	repoOwner    string
	repoName     string
}

func defaultCIPollFixture() ciPollFixture {
	return ciPollFixture{
		inputs: map[string]string{
			executor.InputKind:            executor.KindCIPoll,
			executor.InputPRNumber:        "41",
			executor.InputPollIntervalSec: "1ms",
			executor.InputPollTimeoutSec:  "5s",
		},
		capabilities: []string{string(capability.ProviderPRWrite)},
		repoOwner:    "acme",
		repoName:     "web",
	}
}

func (f ciPollFixture) envelope() apiv1.InvocationEnvelope {
	inputs := make(map[string]interface{}, len(f.inputs))
	for key, value := range f.inputs {
		inputs[key] = value
	}
	return apiv1.InvocationEnvelope{
		Inputs:       inputs,
		Capabilities: f.capabilities,
		RepoRef:      apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: f.repoOwner, Name: f.repoName},
	}
}

// runLocalCIPoll runs the fixture through the in-process kind executor the
// self runner registers, and projects whatever it returns into the single
// envelope a caller can compare — an error included, because the local seam
// reports a retryable failure as an error rather than a failure envelope
// (invoke.InfrastructureFailure) and that projection difference is exactly
// what a pod cannot express.
func runLocalCIPoll(t *testing.T, f ciPollFixture, poller executor.PRPoller) (apiv1.ResultEnvelope, error) {
	t.Helper()
	rec := &recordingArtifactRecorder{}
	ciPoll, err := executor.NewCIPollExecutor(poller, rec)
	if err != nil {
		t.Fatalf("NewCIPollExecutor: %v", err)
	}
	kind := executor.NewCIPollKindExecutor(ciPoll)
	return kind.Run(context.Background(), f.envelope(), apiv1.DeterministicRun{})
}

// runPodCIPoll drives runCIPollStage exactly as a dispatched pod would: the
// dispatcher's stamped environment, a live credential plane, and the shared
// poller seam.
func runPodCIPoll(t *testing.T, f ciPollFixture, poller executor.PRPoller) apiv1.ResultEnvelope {
	t.Helper()
	setPodCIPollEnv(t, f)
	stubPRPoller(t, poller)
	return runCIPollStage(context.Background(), io.Discard)
}

func setPodCIPollEnv(t *testing.T, f ciPollFixture) {
	t.Helper()
	for key, value := range f.inputs {
		t.Setenv(dispatcher.InputEnvVar(key), value)
	}
	encoded, err := json.Marshal(f.capabilities)
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	t.Setenv(dispatcher.EnvStageCapabilities, string(encoded))
	t.Setenv(dispatcher.EnvRunID, "run-ci-poll")
	t.Setenv(dispatcher.EnvStage, "ci-poll")
	t.Setenv(dispatcher.EnvAttempt, "1")
	t.Setenv(dispatcher.EnvStageTimeout, "30s")
	// The dispatcher stamps the run's routed repository for a goobers-CLI
	// stage; ci-poll's placeholder command is one, so a real ci-poll pod
	// carries exactly these.
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, f.repoOwner)
	t.Setenv(executor.RepoNameEnvVar, f.repoName)
	t.Setenv(dispatcher.EnvDaemonAPI, credentialPlaneStub(t, f.capabilities))
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	// No instance root, as in every pod today: the point of C5 is that
	// ci-poll no longer needs one.
	t.Setenv(executor.InstanceRootEnvVar, "")
}

// credentialPlaneStub is the daemon's credential plane, reduced to the one
// route a pod stage calls. It mints a distinct value per capability so a test
// can prove WHICH credential reached the poller.
func credentialPlaneStub(t *testing.T, grant []string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(apicontract.CredentialResolvePath, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Capabilities []string `json:"capabilities"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		granted := map[string]bool{}
		for _, name := range grant {
			granted[name] = true
		}
		var out struct {
			Credentials []dispatcher.MintedCredential `json:"credentials"`
		}
		for _, requested := range req.Capabilities {
			if !granted[requested] {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"capability_undeclared"}`)
				return
			}
			out.Credentials = append(out.Credentials, dispatcher.MintedCredential{
				Capability: requested,
				Value:      "minted-for-" + requested,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// stubPRPoller substitutes the fixture poller on the seam BOTH substrates
// resolve through.
func stubPRPoller(t *testing.T, poller executor.PRPoller) {
	t.Helper()
	prev := newPRPoller
	newPRPoller = func(string) executor.PRPoller { return poller }
	t.Cleanup(func() { newPRPoller = prev })
}

// recordingArtifactRecorder is the LOCAL side's artifact sink. It implements
// both executor.ArtifactRecorder and the integrity recorder CIPollExecutor's
// failing-CI arm asserts on, so the local reference behaves as the runner's
// journal-backed recorder does.
type recordingArtifactRecorder struct {
	names []string
}

func (r *recordingArtifactRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	ref, err := journal.ArtifactRef(data)
	if err != nil {
		return journal.Ref{}, err
	}
	r.names = append(r.names, name)
	return ref, nil
}

func (r *recordingArtifactRecorder) RecordArtifactWithIntegrity(name string, data []byte, _ apiv1.Integrity) (journal.Ref, error) {
	return r.RecordArtifact(name, data)
}

// --- parity: a terminal poll produces one envelope on both substrates -------

// TestCIPollPodMatchesLocalOnAPassingPoll is the P0 parity row for C5: the
// same declaration and the same provider answers produce the same outputs
// whether the stage ran in goobers-api or in a pod. Before #3881 this test
// could not exist at all — the pod side refused the kind before it ran.
func TestCIPollPodMatchesLocalOnAPassingPoll(t *testing.T) {
	fixture := defaultCIPollFixture()
	answers := []providers.PullRequestPollResult{
		{CheckState: providers.CheckStatePending},
		{CheckState: providers.CheckStatePassing},
	}

	local, err := runLocalCIPoll(t, fixture, &fakeCIPoller{results: answers})
	if err != nil {
		t.Fatalf("local ci-poll: %v", err)
	}
	// PREMISE (ungraded): the local side really does report the terminal check
	// state in the providers vocabulary, so a matching pod side means
	// something. A vacuous pair of empty outputs would otherwise "agree".
	if local.Status != apiv1.ResultSuccess || local.Outputs[executor.OutputCIStatus] != string(providers.CheckStatePassing) {
		t.Fatalf("local ci-poll = %+v, want a success carrying ciStatus=passing", local)
	}

	pod := runPodCIPoll(t, fixture, &fakeCIPoller{results: answers})
	if pod.Status != local.Status {
		t.Fatalf("pod status = %q, local = %q", pod.Status, local.Status)
	}
	if !reflect.DeepEqual(pod.Outputs, local.Outputs) {
		t.Fatalf("pod outputs = %#v, local = %#v", pod.Outputs, local.Outputs)
	}
	if pod.Summary != local.Summary {
		t.Fatalf("pod summary = %q, local = %q", pod.Summary, local.Summary)
	}
}

// TestCIPollPodMatchesLocalOnAFailingPoll pins the arm that needs the
// artifact recorder: a failing poll records ci-checks.json through an
// INTEGRITY recorder, and the assertion for it is a type switch that fails at
// run time. A pod recorder missing that method failed exactly the polls the
// evidence exists for while passing polls succeeded — the kind of gap that
// only shows up on a red lane.
func TestCIPollPodMatchesLocalOnAFailingPoll(t *testing.T) {
	fixture := defaultCIPollFixture()
	answers := []providers.PullRequestPollResult{{
		CheckState: providers.CheckStateFailing,
		Checks: []providers.CheckDetail{
			{Name: "build (ubuntu)", State: providers.CheckStateFailing, Summary: "go build failed"},
		},
	}}

	local, err := runLocalCIPoll(t, fixture, &fakeCIPoller{results: answers})
	if err != nil {
		t.Fatalf("local ci-poll: %v", err)
	}
	if local.Outputs[executor.OutputCIStatus] != string(providers.CheckStateFailing) {
		t.Fatalf("local ci-poll = %+v, want ciStatus=failing", local)
	}

	pod := runPodCIPoll(t, fixture, &fakeCIPoller{results: answers})
	if pod.Status != local.Status {
		t.Fatalf("pod status = %q, local = %q", pod.Status, local.Status)
	}
	if pod.Outputs[executor.OutputCIStatus] != local.Outputs[executor.OutputCIStatus] {
		t.Fatalf("pod ciStatus = %v, local = %v", pod.Outputs[executor.OutputCIStatus], local.Outputs[executor.OutputCIStatus])
	}
	if pod.Outputs[executor.OutputCIFailedChecks] != local.Outputs[executor.OutputCIFailedChecks] {
		t.Fatalf("pod ciFailedChecks = %v, local = %v", pod.Outputs[executor.OutputCIFailedChecks], local.Outputs[executor.OutputCIFailedChecks])
	}
	if len(pod.Artifacts) != 1 || pod.Artifacts[0].Path == "" {
		t.Fatalf("pod artifacts = %#v, want the ci-checks evidence pointer the local side also produced (%#v)", pod.Artifacts, local.Artifacts)
	}
}

// TestCIPollPodDefaultsOwnerAndRepoFromTheRoutedRepository pins the
// GOOBERS_REPO_* half of the envelope rebuild: the shipped implementation.yaml
// declares neither prOwner nor prRepo and relies on the run's RepoRef, so a
// pod that dropped the routed repository would poll owner ""/repo "" and fail
// with a config error that names nothing.
func TestCIPollPodDefaultsOwnerAndRepoFromTheRoutedRepository(t *testing.T) {
	fixture := defaultCIPollFixture()
	poller := &recordingPRPoller{result: providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}}

	if result := runPodCIPoll(t, fixture, poller); result.Status != apiv1.ResultSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	if poller.request.Repository.Owner != "acme" || poller.request.Repository.Name != "web" {
		t.Fatalf("polled %+v, want acme/web from the routed repository", poller.request.Repository)
	}
	if poller.request.PullID != "41" {
		t.Fatalf("polled pull %q, want 41 from the declared prNumber input", poller.request.PullID)
	}
}

type recordingPRPoller struct {
	request providers.PullRequestPollRequest
	result  providers.PullRequestPollResult
	err     error
}

func (p *recordingPRPoller) PollPullRequest(_ context.Context, req providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	p.request = req
	return p.result, p.err
}

// --- the rate-limit report (the half of the quota observer a pod can send) --

// TestCIPollPodSurfacesRateLimitedWithReset is C5's own acceptance: a
// rate-limited poll from a pod carries providers.ErrorCodeRateLimited AND
// rateLimitReset, because that pair is the ONLY channel by which a pod can
// tell the daemon's ProviderQuotaState the window is exhausted (the
// in-process QuotaObserver the self runner wires does not exist in a pod).
// The reset is on OUTPUTS, not in the message: internal/runner's
// outputRateLimitReset parses that key on its way to
// ProviderQuotaState.RecordExhausted, and a reset only a human can read
// records nothing.
func TestCIPollPodSurfacesRateLimitedWithReset(t *testing.T) {
	fixture := defaultCIPollFixture()
	// One retry's worth of budget, so the executor gives up on the rate limit
	// rather than polling until the fixture's timeout.
	fixture.inputs[executor.InputPollTimeoutSec] = "1s"
	reset := time.Now().Add(37 * time.Minute).UTC().Truncate(time.Second)
	rateLimited := fmt.Errorf("poll checks: %w", &providers.RateLimitError{
		Endpoint: "GET /repos/acme/web/pulls/41", Status: 403, Reset: reset,
	})
	poller := &fakeCIPoller{errs: []error{rateLimited}}

	result := runPodCIPoll(t, fixture, poller)
	if result.Status != apiv1.ResultFailure || result.Error == nil {
		t.Fatalf("result = %+v, want a failure envelope", result)
	}
	if result.Error.Code != providers.ErrorCodeRateLimited {
		t.Fatalf("error code = %q, want %q", result.Error.Code, providers.ErrorCodeRateLimited)
	}
	if !result.Error.Retryable {
		t.Fatal("a rate-limited poll must be retryable; the window rolls over")
	}
	got, ok := result.Outputs[executor.OutputRateLimitReset].(string)
	if !ok {
		t.Fatalf("outputs = %#v, want a %s the daemon's RateLimited observer can parse", result.Outputs, executor.OutputRateLimitReset)
	}
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("parse %s %q: %v", executor.OutputRateLimitReset, got, err)
	}
	if !parsed.Equal(reset) {
		t.Fatalf("%s = %s, want %s", executor.OutputRateLimitReset, parsed, reset)
	}
}

// TestCIPollRateLimitCodeIsTheSameOnBothSubstrates is the parity half of the
// row above. The pod cannot report a rate limit the way the self runner does
// (an infrastructure-classed ERROR across the invoke seam, plus an in-process
// quota observation), but it MUST report it under the same NAME — a lane that
// journaled github_rate_limited locally and something else in a pod would
// split one operator question across two queries.
func TestCIPollRateLimitCodeIsTheSameOnBothSubstrates(t *testing.T) {
	fixture := defaultCIPollFixture()
	fixture.inputs[executor.InputPollTimeoutSec] = "1s"
	rateLimited := fmt.Errorf("poll checks: %w", &providers.RateLimitError{Reset: time.Now().Add(time.Hour)})

	// PREMISE (ungraded): the local side really does classify this as a
	// retryable, rate-limit-named failure rather than a generic one.
	_, localErr := runLocalCIPoll(t, fixture, &fakeCIPoller{errs: []error{rateLimited}})
	if localErr == nil {
		t.Fatal("local ci-poll returned no error for a rate-limited poll")
	}
	var coded interface{ StageErrorCode() string }
	if !errors.As(localErr, &coded) {
		t.Fatalf("local ci-poll error %v carries no stage code", localErr)
	}
	if coded.StageErrorCode() != providers.ErrorCodeRateLimited {
		t.Fatalf("local stage code = %q, want %q", coded.StageErrorCode(), providers.ErrorCodeRateLimited)
	}

	pod := runPodCIPoll(t, fixture, &fakeCIPoller{errs: []error{rateLimited}})
	if pod.Error == nil || pod.Error.Code != coded.StageErrorCode() {
		t.Fatalf("pod error = %+v, want code %q to match the local stage code", pod.Error, coded.StageErrorCode())
	}
}

// TestCIPollPodOmitsRateLimitResetWhenTheProviderSentNone: a rate limit with
// no reset header reports no key at all. A zero timestamp would be parsed
// successfully by the observer and park ProviderQuotaState at the epoch —
// strictly worse than the absence the consumers already skip.
func TestCIPollPodOmitsRateLimitResetWhenTheProviderSentNone(t *testing.T) {
	fixture := defaultCIPollFixture()
	fixture.inputs[executor.InputPollTimeoutSec] = "1s"
	poller := &fakeCIPoller{errs: []error{fmt.Errorf("poll: %w", &providers.RateLimitError{})}}

	result := runPodCIPoll(t, fixture, poller)
	if result.Error == nil || result.Error.Code != providers.ErrorCodeRateLimited {
		t.Fatalf("result = %+v, want a rate-limited failure", result)
	}
	if _, present := result.Outputs[executor.OutputRateLimitReset]; present {
		t.Fatalf("outputs = %#v, want no %s when the provider sent none", result.Outputs, executor.OutputRateLimitReset)
	}
}

// TestCIPollPodClassifiesATerminalProviderFailure: a non-transient provider
// failure is a terminal business outcome on both substrates, named the same
// and NOT retryable — retrying a 401 with the same credential cannot succeed.
func TestCIPollPodClassifiesATerminalProviderFailure(t *testing.T) {
	fixture := defaultCIPollFixture()
	poller := &fakeCIPoller{errs: []error{errors.New("GET /pulls/41 failed: status 401: bad credentials")}}

	local, localErr := runLocalCIPoll(t, fixture, &fakeCIPoller{errs: []error{errors.New("GET /pulls/41 failed: status 401: bad credentials")}})
	if localErr != nil {
		t.Fatalf("local ci-poll: %v", localErr)
	}
	if local.Error == nil || local.Error.Code != executor.CIPollProviderErrorCode {
		t.Fatalf("local result = %+v, want %q", local, executor.CIPollProviderErrorCode)
	}

	pod := runPodCIPoll(t, fixture, poller)
	if pod.Status != local.Status {
		t.Fatalf("pod status = %q, local = %q", pod.Status, local.Status)
	}
	if pod.Error == nil || pod.Error.Code != local.Error.Code {
		t.Fatalf("pod error = %+v, want code %q", pod.Error, local.Error.Code)
	}
	if pod.Error.Retryable {
		t.Fatal("a terminal provider failure must not be marked retryable")
	}
}

// --- credential-plane refusals ---------------------------------------------

// TestCIPollPodRefusesWithoutTheDeclaredCapability: a ci-poll stage that never
// declared provider:pr:write is refused BEFORE the credential plane is asked
// for anything, and under a code that blames the declaration rather than the
// plane. The in-process executor refuses the same shape
// (internal/executor/dispatch.go), so this is a refusal both substrates make.
func TestCIPollPodRefusesWithoutTheDeclaredCapability(t *testing.T) {
	fixture := defaultCIPollFixture()
	fixture.capabilities = nil

	// PREMISE (ungraded): the local side refuses it too.
	if _, err := runLocalCIPoll(t, fixture, &fakeCIPoller{results: []providers.PullRequestPollResult{{CheckState: providers.CheckStatePassing}}}); err == nil {
		t.Fatal("local ci-poll ran without a declared provider:pr:write")
	}

	poller := &recordingPRPoller{result: providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}}
	result := runPodCIPoll(t, fixture, poller)
	if result.Status != apiv1.ResultFailure || result.Error == nil {
		t.Fatalf("result = %+v, want a refusal", result)
	}
	if result.Error.Code != ciPollCapabilityUndeclaredCode {
		t.Fatalf("error code = %q, want %q", result.Error.Code, ciPollCapabilityUndeclaredCode)
	}
	if poller.request.PullID != "" {
		t.Fatal("an undeclared ci-poll reached the provider")
	}
	if !strings.Contains(result.Error.Message, string(capability.ProviderPRWrite)) {
		t.Fatalf("error message = %q, want it to name the missing capability", result.Error.Message)
	}
}

// TestCIPollPodFailsClosedWhenTheCredentialPlaneRefuses: a plane that answers
// 403 leaves the stage UNCREDENTIALED, and an uncredentialed poll would fail
// far away, against the provider, with a 401 that blames GitHub. Fail closed
// here, naming the plane.
func TestCIPollPodFailsClosedWhenTheCredentialPlaneRefuses(t *testing.T) {
	fixture := defaultCIPollFixture()
	poller := &recordingPRPoller{result: providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}}
	setPodCIPollEnv(t, fixture)
	stubPRPoller(t, poller)
	// A plane that grants nothing: the declared capability is refused.
	t.Setenv(dispatcher.EnvDaemonAPI, credentialPlaneStub(t, nil))

	result := runCIPollStage(context.Background(), io.Discard)
	if result.Status != apiv1.ResultFailure || result.Error == nil {
		t.Fatalf("result = %+v, want a failure", result)
	}
	if result.Error.Code != "credential_resolve_failed" {
		t.Fatalf("error code = %q, want credential_resolve_failed", result.Error.Code)
	}
	if poller.request.PullID != "" {
		t.Fatal("an uncredentialed ci-poll reached the provider")
	}
}

// TestCIPollPodFailsClosedWithoutACredentialPlane: no GOOBERS_DAEMON_API means
// there is no route to a credential at all. The stage must not proceed
// tokenless.
func TestCIPollPodFailsClosedWithoutACredentialPlane(t *testing.T) {
	fixture := defaultCIPollFixture()
	poller := &recordingPRPoller{result: providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}}
	setPodCIPollEnv(t, fixture)
	stubPRPoller(t, poller)
	t.Setenv(dispatcher.EnvDaemonAPI, "")

	result := runCIPollStage(context.Background(), io.Discard)
	if result.Error == nil || result.Error.Code != "credential_resolve_failed" {
		t.Fatalf("result = %+v, want credential_resolve_failed", result)
	}
	if poller.request.PullID != "" {
		t.Fatal("an uncredentialed ci-poll reached the provider")
	}
}

// TestCIPollPodPollsWithTheResolvedCredential proves the credential that
// reaches the poller is the one the PLANE minted for provider:pr:write — not
// an ambient token, and not the checkout credential.
func TestCIPollPodPollsWithTheResolvedCredential(t *testing.T) {
	fixture := defaultCIPollFixture()
	setPodCIPollEnv(t, fixture)
	var seen string
	prev := newPRPoller
	newPRPoller = func(token string) executor.PRPoller {
		seen = token
		return &recordingPRPoller{result: providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}}
	}
	t.Cleanup(func() { newPRPoller = prev })

	if result := runCIPollStage(context.Background(), io.Discard); result.Status != apiv1.ResultSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	if want := "minted-for-" + string(capability.ProviderPRWrite); seen != want {
		t.Fatalf("poller token = %q, want %q", seen, want)
	}
}

// --- the refusal list, from the pod's side ---------------------------------

// TestRunStageRoutesCIPollToTheInProcessExecutor: dispatch-exec must select
// the KIND, not shell out the placeholder command. Running ["goobers",
// "ci-poll"] would exit nonzero with "unknown command" and surrender a
// meaningless stage_failed.
func TestRunStageRoutesCIPollToTheInProcessExecutor(t *testing.T) {
	fixture := defaultCIPollFixture()
	setPodCIPollEnv(t, fixture)
	// The placeholder command a real ci-poll stage declares.
	t.Setenv(dispatcher.EnvStageCommand, `["goobers","ci-poll"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvAgenticKitDigest, "")
	stubPRPoller(t, &recordingPRPoller{result: providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}})

	outcome := runStage(context.Background(), io.Discard, io.Discard)
	if outcome.Result.Status != apiv1.ResultSuccess {
		t.Fatalf("result = %+v, want the in-process ci-poll's success", outcome.Result)
	}
	if outcome.Result.Outputs[executor.OutputCIStatus] != string(providers.CheckStatePassing) {
		t.Fatalf("outputs = %#v, want ciStatus=passing", outcome.Result.Outputs)
	}
	if outcome.Verdict != nil {
		t.Fatal("a deterministic ci-poll surrendered a verdict")
	}
}

// TestRunDeclaredStageNoLongerRefusesCIPoll is the ablation for the refusal
// change: with ci-poll off the list, the pod-entrypoint backstop must not
// refuse it. It is asserted on runDeclaredStage — the backstop's own function
// — so a future refactor that re-adds the kind to the list is caught here
// even if runStage's routing hides it.
func TestRunDeclaredStageNoLongerRefusesCIPoll(t *testing.T) {
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","true"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")
	t.Setenv(executor.InstanceRootEnvVar, "")
	t.Setenv(dispatcher.InputEnvVar(executor.InputKind), executor.KindCIPoll)

	result := runDeclaredStage(context.Background(), io.Discard, io.Discard)
	if result.Error != nil && result.Error.Code == executor.StageRequiresInstanceRootCode {
		t.Fatalf("result = %+v, ci-poll runs in a pod since #3881 and must not trip the instance-root backstop", result)
	}
}

// --- providers a pod cannot resolve a poller for ---------------------------

// TestCIPollPodRefusesANonGitHubProvider: ADO and Gitea both resolve part of
// their poller identity from the instance config directory, which a pod does
// not have. Refused loudly rather than defaulting to GitHub — a silent
// default would poll a real api.github.com repository that merely shares the
// routed owner/name, and report ITS check state as this run's.
func TestCIPollPodRefusesANonGitHubProvider(t *testing.T) {
	for _, provider := range []providers.ProviderKind{providers.ProviderADO, providers.ProviderGitea} {
		t.Run(string(provider), func(t *testing.T) {
			fixture := defaultCIPollFixture()
			poller := &recordingPRPoller{result: providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}}
			setPodCIPollEnv(t, fixture)
			stubPRPoller(t, poller)
			t.Setenv(executor.RepoProviderEnvVar, string(provider))
			if provider == providers.ProviderADO {
				t.Setenv(executor.RepoProjectEnvVar, "acme-project")
			}

			result := runCIPollStage(context.Background(), io.Discard)
			if result.Status != apiv1.ResultFailure || result.Error == nil {
				t.Fatalf("result = %+v, want a refusal", result)
			}
			if result.Error.Code != ciPollProviderUnsupportedCode {
				t.Fatalf("error code = %q, want %q", result.Error.Code, ciPollProviderUnsupportedCode)
			}
			if poller.request.PullID != "" {
				t.Fatalf("a %s ci-poll reached the GitHub poller", provider)
			}
		})
	}
}

// --- declaration faults -----------------------------------------------------

// TestCIPollPodNamesAMissingPRNumberAsADeclarationFault: prNumber arrives
// through inputsFrom from open-pr's outputs. A missing one is a workflow
// problem, and coding it as poll_provider_error would send an operator to the
// provider's status page for a typo in a mapping.
func TestCIPollPodNamesAMissingPRNumberAsADeclarationFault(t *testing.T) {
	fixture := defaultCIPollFixture()
	delete(fixture.inputs, executor.InputPRNumber)
	poller := &recordingPRPoller{result: providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}}

	result := runPodCIPoll(t, fixture, poller)
	if result.Error == nil || result.Error.Code != "stage_declaration_invalid" {
		t.Fatalf("result = %+v, want stage_declaration_invalid", result)
	}
	if poller.request.PullID != "" {
		t.Fatal("an undeclared ci-poll reached the provider")
	}
}
