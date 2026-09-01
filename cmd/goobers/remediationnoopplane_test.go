package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/stateclient"
)

// remediationnoopplane_test.go is Goobers#3989's evidence: the pr-remediation
// no-op guard — the record that stops the lane re-attempting a PR whose
// previous attempt already concluded there was nothing to do — reaches the SAME
// record from the daemon's own process and from a stage pod, over all three
// planes it needs.
//
// The failure this file exists to prevent is not a crash. In a pod
// GOOBERS_INSTANCE_ROOT is unset, so before this change the absent record read
// as "no prior no-op" on every run: the guard failed OPEN, silently, and the
// lane looped on the same PR burning a full agentic cycle each time. Every test
// below therefore asserts either that the two sides observe one record, or that
// a route that cannot serve one says so out loud.

const noopPlaneGaggle = "goobers"

// noopPodLayout is a stage pod's layout: a root with NOTHING in it. Any test
// that finds a scheduler directory under it has caught the guard falling back
// to a local file the pod does not really have.
func noopPodLayout(t *testing.T) instance.Layout {
	t.Helper()
	return instance.NewLayout(t.TempDir())
}

func assertNoPodSchedulerDir(t *testing.T, pod instance.Layout) {
	t.Helper()
	if _, err := os.Stat(pod.SchedulerDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the pod created %s (err = %v); a plane-routed guard must never touch a local scheduler directory",
			pod.SchedulerDir(), err)
	}
}

// TestRemediationNoopStateKeyIsAClosedKeyedShape pins the C2 half of the fix.
//
// The record used to live in ONE fixed pr-remediation-noop.json holding a map
// of every PR — a name stateclient.ValidKey does not admit, which is precisely
// why the plane could not serve it. It is now one key per (gaggle, PR), and the
// properties that matter are: the namespace admits it, it is a bare file name
// in the scheduler directory (not a second path element), it is a pure function
// of the record's identity, and DIFFERENT PRs and different gaggles never
// collide onto one key.
func TestRemediationNoopStateKeyIsAClosedKeyedShape(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())

	key := remediationNoopStateKey(remediationNoopKey(noopPlaneGaggle, 77))
	if !stateclient.ValidKey(key) {
		t.Fatalf("key %q is outside the closed scheduler-state namespace", key)
	}
	relative, err := stateclient.KeyRelativePath(key)
	if err != nil {
		t.Fatal(err)
	}
	if relative != key {
		t.Fatalf("KeyRelativePath(%q) = %q, want the bare file name", key, relative)
	}
	if got, want := filepath.Join(layout.SchedulerDir(), relative),
		filepath.Join(layout.SchedulerDir(), key); got != want {
		t.Fatalf("key resolves to %s, want %s", got, want)
	}

	// Deterministic: the daemon and the pod must derive the same key from the
	// same record, or they are guarding two different PRs.
	if again := remediationNoopStateKey(remediationNoopKey(noopPlaneGaggle, 77)); again != key {
		t.Fatalf("key is not a pure function of the record: %q then %q", key, again)
	}

	// Distinct per PR and per gaggle. The unscoped ("") gaggle is the shape a
	// pre-GAG-011 instance still writes, so it must not alias a named one.
	seen := map[string]string{}
	for _, record := range []string{
		remediationNoopKey(noopPlaneGaggle, 77),
		remediationNoopKey(noopPlaneGaggle, 78),
		remediationNoopKey("other", 77),
		remediationNoopKey("", 77),
	} {
		stateKey := remediationNoopStateKey(record)
		if previous, collided := seen[stateKey]; collided {
			t.Fatalf("records %q and %q share state key %q", previous, record, stateKey)
		}
		seen[stateKey] = record
	}

	// The LEGACY aggregate name is emphatically not a key: admitting it would
	// hand a state bearer the whole instance's records in one read.
	if stateclient.ValidKey(legacyRemediationNoopStateFile) {
		t.Fatalf("the pre-#3989 aggregate %q is a valid state key", legacyRemediationNoopStateFile)
	}
}

// TestRemediationNoopKeyRidesTheClaimsLock is the mutual exclusion the issue
// requires be PRESERVED through the plane route.
//
// The guard has always serialized on claims.lock, and it has to keep doing so:
// terminal cleanup folds a no-op into the record while holding that lock to
// read the run's PR claim, so a no-op key on any other lock would put the read
// and the write in two atomicity domains. schedulerStateLock's default arm is
// claims.lock, and this is the pin that keeps the key falling through to it.
func TestRemediationNoopKeyRidesTheClaimsLock(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := schedulerStateLock(layout)
	key := remediationNoopStateKey(remediationNoopKey(noopPlaneGaggle, 77))
	if err := lock(key, remediationNoopLockOperation, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(layout.SchedulerDir(), claimLockFileName)); err != nil {
		t.Fatalf("the no-op record did not take %s: %v", claimLockFileName, err)
	}
	for _, other := range []string{postMergeReconcileLockFile, siblingCacheLockFileName, backlogHealthCursorLockFile} {
		if _, err := os.Stat(filepath.Join(layout.SchedulerDir(), other)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the no-op record took %s as well (err = %v); it must ride claims.lock alone", other, err)
		}
	}
}

// TestRemediationNoopFileBackendWritesUnderTheSchedulerDirectory is the
// local-path compatibility check: on a type-1/type-2 host the record is still
// one JSON file in the instance's own scheduler directory, so an operator
// looking for it — and the daemon reading it in-process — find it where every
// other scheduler-state key lives.
func TestRemediationNoopFileBackendWritesUnderTheSchedulerDirectory(t *testing.T) {
	t.Setenv(stateclient.EnvEndpoint, "")
	l := layoutFor(initDemo(t))
	key := remediationNoopKey("", 77)
	signature := remediationNoopSignature{HeadSHA: "head-a", Causes: "substantive"}

	store, err := remediationNoopStore(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, isFile := store.(*stateclient.File); !isFile {
		t.Fatalf("store = %T off the plane, want the file backend", store)
	}
	if err := updateRemediationNoopState(t.Context(), store, key, signature, "run-1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(l.SchedulerDir(), remediationNoopStateKey(key))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the record is not at %s: %v", path, err)
	}
	// And it went through the claims lock on the way, exactly as the
	// pre-plane writer did.
	if _, err := os.Stat(filepath.Join(l.SchedulerDir(), claimLockFileName)); err != nil {
		t.Fatalf("the file backend did not take %s: %v", claimLockFileName, err)
	}
}

// TestRemediationNoopRecordCrossesTheDaemonPodBoundary is the seam test the
// issue asks for in both directions: a pod observes a no-op recorded by the
// daemon, and the daemon observes one recorded by the pod.
func TestRemediationNoopRecordCrossesTheDaemonPodBoundary(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, noopPlaneGaggle, "run-pod")
	pod := noopPodLayout(t)
	key := remediationNoopKey(noopPlaneGaggle, 77)
	signature := remediationNoopSignature{HeadSHA: "head-a", DiffDigest: "sha256:diff-a"}

	// 1. The DAEMON records the first no-op in its own process, on the file
	//    backend under claims.lock — the pre-plane path, unchanged.
	if err := updateRemediationNoopState(
		t.Context(), plane.daemonStore(t), key, signature, "run-daemon"); err != nil {
		t.Fatal(err)
	}

	// 2. A POD counts its own attempt. Before #3989 it would have seen an
	//    absent record and started again at one — the silent fail-open.
	stampStatePlaneEnv(t, plane, noopPlaneGaggle, token)
	record, reset, err := recordGatherPRContextDigestNoop(pod, 77, signature, "run-pod", false)
	if err != nil {
		t.Fatalf("pod record: %v", err)
	}
	if reset || record.Attempts != 2 || record.LastRunID != "run-pod" {
		t.Fatalf("pod record = %+v, reset = %v; want the daemon's attempt carried forward", record, reset)
	}
	assertNoPodSchedulerDir(t, pod)

	// 3. The DAEMON sees the pod's increment on the very same file.
	daemonRecord, err := readRemediationNoopRecord(t.Context(), plane.daemonStore(t), key)
	if err != nil {
		t.Fatal(err)
	}
	if daemonRecord != record {
		t.Fatalf("daemon reads %+v, pod wrote %+v; the two sides are not one record", daemonRecord, record)
	}

	// 4. The POD parks it; the daemon sees the park.
	if err := markRemediationNoopParked(pod, key); err != nil {
		t.Fatal(err)
	}
	daemonRecord, err = readRemediationNoopRecord(t.Context(), plane.daemonStore(t), key)
	if err != nil {
		t.Fatal(err)
	}
	if !daemonRecord.Parked {
		t.Fatalf("daemon record = %+v, want the pod's park visible", daemonRecord)
	}

	// 5. And the other way: the DAEMON clears the record (a run that finally
	//    made progress), and the pod's next read observes an empty guard
	//    rather than a stale streak that would park a healthy PR.
	if err := clearRemediationNoopState(t.Context(), plane.daemonStore(t), key); err != nil {
		t.Fatal(err)
	}
	podRecord, err := remediationNoopRecordForSignature(pod, 77, signature)
	if err != nil {
		t.Fatal(err)
	}
	if !podRecord.empty() {
		t.Fatalf("pod record = %+v after the daemon cleared it, want an empty guard", podRecord)
	}
	assertNoPodSchedulerDir(t, pod)
}

// TestPodAndDaemonNoopWritesShareOneAtomicityDomain is the interleaving case.
//
// Each distinct run counts exactly one attempt, so the final count is an exact
// number rather than a range: if the plane's compare-and-swap and the file
// backend's claims.lock were separate atomicity domains, increments would be
// lost and the guard would under-count — which is to say it would let the lane
// keep re-attempting a PR past its limit.
func TestPodAndDaemonNoopWritesShareOneAtomicityDomain(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, noopPlaneGaggle, "run-engine")
	key := remediationNoopKey(noopPlaneGaggle, 77)
	signature := remediationNoopSignature{HeadSHA: "head-a", Causes: "substantive"}

	pod := plane.client(t, noopPlaneGaggle, token)
	daemon := plane.daemonStore(t)

	const perWriter = 10
	count := func(store stateclient.Store, prefix string) error {
		for i := 0; i < perWriter; i++ {
			if err := updateRemediationNoopState(
				t.Context(), store, key, signature, prefix+strconv.Itoa(i)); err != nil {
				return err
			}
		}
		return nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	prefixes := []string{"pod-run-", "daemon-run-"}
	for index, store := range []stateclient.Store{pod, daemon} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[index] = count(store, prefixes[index])
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	record, err := readRemediationNoopRecord(t.Context(), daemon, key)
	if err != nil {
		t.Fatal(err)
	}
	if record.Attempts != 2*perWriter {
		t.Fatalf("attempts = %d, want %d — an increment was lost across the two writers",
			record.Attempts, 2*perWriter)
	}
}

// TestStatePlaneRefusesNoopKeyLookalikes is the key-validation half of the
// containment: the digest shape is pinned server-side, so a state bearer cannot
// walk out of the namespace even if the client is hostile. The legacy aggregate
// name is in the list deliberately — it is the one lookalike that, if admitted,
// would hand a pod EVERY PR's record in a single read.
func TestStatePlaneRefusesNoopKeyLookalikes(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, noopPlaneGaggle, "run-1")

	// Real files in the scheduler directory the plane must never serve.
	for name, body := range map[string]string{
		claimLedgerFileName:            `{"claims":"secret"}`,
		legacyRemediationNoopStateFile: `{"records":{"other/pr/1":{"attempts":9}}}`,
	} {
		if err := os.WriteFile(filepath.Join(plane.layout.SchedulerDir(), name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	digest := strings.Repeat("ab", 32)
	for _, key := range []string{
		legacyRemediationNoopStateFile,
		"pr-remediation-noop-.json",
		"pr-remediation-noop-" + digest[:63] + ".json",
		"pr-remediation-noop-" + digest + "x.json",
		"pr-remediation-noop-" + digest + ".json.tmp",
		"pr-remediation-noop-" + strings.ToUpper(digest) + ".json",
		"pr-remediation-noop-" + digest + ".json/../claims.json",
		"Pr-remediation-noop-" + digest + ".json",
		"pr-remediation-noop-" + digest,
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			plane.server.URL+"/api/v1/gaggles/"+noopPlaneGaggle+"/state/"+key, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		switch response.StatusCode {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusForbidden:
		default:
			t.Fatalf("key %q status = %d body = %s, want it refused",
				key, response.StatusCode, string(body))
		}
		if strings.Contains(string(body), "secret") || strings.Contains(string(body), `"attempts":9`) {
			t.Fatalf("key %q leaked scheduler-directory contents: %s", key, string(body))
		}
	}

	// The well-formed key still works for the same bearer, so the refusals
	// above are about the key and not about the route being broken.
	valid := remediationNoopStateKey(remediationNoopKey(noopPlaneGaggle, 77))
	if _, err := plane.client(t, noopPlaneGaggle, token).Put(t.Context(), valid, []byte(`{}`), ""); err != nil {
		t.Fatalf("well-formed key write: %v", err)
	}
}

// TestStatePlaneRefusesNoopReadsWithoutAValidStateBearer is the auth-isolation
// half. The scheduler-state plane has its own narrowly scoped bearer for the
// same reason the claims plane does: a CLI subprocess must not be able to
// present the pod's surrender token — or nothing at all — and be served
// gaggle-wide state.
func TestStatePlaneRefusesNoopReadsWithoutAValidStateBearer(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, noopPlaneGaggle, "run-1")
	key := remediationNoopStateKey(remediationNoopKey(noopPlaneGaggle, 77))

	// Seed a real record through an authorized client so an unauthorized read
	// would have something to leak.
	if _, err := plane.client(t, noopPlaneGaggle, token).Put(
		t.Context(), key, []byte(`{"schema":"probe","record":{"attempts":4}}`), ""); err != nil {
		t.Fatal(err)
	}

	for name, bearer := range map[string]string{
		"no bearer":       "",
		"empty bearer":    "Bearer ",
		"forged bearer":   "Bearer not-a-minted-token",
		"wrong scheme":    "Basic " + token,
		"truncated token": "Bearer " + token[:len(token)/2],
	} {
		t.Run(name, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				request, err := http.NewRequestWithContext(t.Context(), method,
					plane.server.URL+"/api/v1/gaggles/"+noopPlaneGaggle+"/state/"+key, strings.NewReader(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				if bearer != "" {
					request.Header.Set("Authorization", bearer)
				}
				request.Header.Set("If-Match", `"deadbeef"`)
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
					t.Fatalf("%s status = %d, want 401/403", method, response.StatusCode)
				}
				if strings.Contains(string(body), "attempts") {
					t.Fatalf("%s leaked the record: %s", method, string(body))
				}
			}
		})
	}

	// The record is untouched by every refusal above.
	record, err := plane.client(t, noopPlaneGaggle, token).Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(record.Data), `"attempts":4`) {
		t.Fatalf("record = %s, want the seeded value intact", string(record.Data))
	}
}

// TestRemediationNoopGuardFailsClosedOnAHalfConfiguredStatePlane is the
// property the whole issue turns on, applied to every stage-side entry point:
// an endpoint with no bearer (or no gaggle) must be an ERROR, never a
// fall-through to a local file. A pod that fell through would read "no prior
// no-op" and loop the lane; a pod that errors stops it.
func TestRemediationNoopGuardFailsClosedOnAHalfConfiguredStatePlane(t *testing.T) {
	signature := remediationNoopSignature{HeadSHA: "head-a", DiffDigest: "sha256:diff-a"}

	cases := []struct {
		name          string
		token, gaggle string
		want          error
	}{
		{name: "endpoint without a bearer", gaggle: noopPlaneGaggle, want: stateclient.ErrEndpointWithoutToken},
		{name: "endpoint without a gaggle", token: "state-token", want: stateclient.ErrEndpointWithoutGaggle},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pod := noopPodLayout(t)
			t.Setenv(stateclient.EnvEndpoint, "http://daemon.invalid:7777")
			t.Setenv(stateclient.EnvToken, testCase.token)
			t.Setenv(stateclient.EnvGaggle, testCase.gaggle)

			key := remediationNoopKey(testCase.gaggle, 77)
			if _, _, err := recordGatherPRContextDigestNoop(pod, 77, signature, "run-1", false); !errors.Is(err, testCase.want) {
				t.Fatalf("recordGatherPRContextDigestNoop err = %v, want %v", err, testCase.want)
			}
			if _, err := remediationNoopRecordForSignature(pod, 77, signature); !errors.Is(err, testCase.want) {
				t.Fatalf("remediationNoopRecordForSignature err = %v, want %v", err, testCase.want)
			}
			if err := markRemediationNoopParked(pod, key); !errors.Is(err, testCase.want) {
				t.Fatalf("markRemediationNoopParked err = %v, want %v", err, testCase.want)
			}
			if err := clearRemediationNoopRecord(pod, key); !errors.Is(err, testCase.want) {
				t.Fatalf("clearRemediationNoopRecord err = %v, want %v", err, testCase.want)
			}
			assertNoPodSchedulerDir(t, pod)
		})
	}
}

// TestRecordPRRemediationNoopSurfacesJournalSeamFailures is the C4 half's
// fail-closed property. The terminal writer decides whether a run was a
// remediation no-op by reading that run's journal; a read it could not perform
// must stop the caller, never look like a run that said nothing — which would
// silently drop the attempt that was supposed to be counted.
func TestRecordPRRemediationNoopSurfacesJournalSeamFailures(t *testing.T) {
	l := layoutFor(initDemo(t))

	// An endpoint with no bearer: journalclient.Select's refusal.
	setPlaneEnv(t, "http://daemon.invalid:7777", "", "run-1", noopPlaneGaggle)
	err := recordPRRemediationNoop(l, "run-1")
	if !errors.Is(err, journalclient.ErrEndpointWithoutToken) {
		t.Fatalf("err = %v, want ErrEndpointWithoutToken", err)
	}

	// An endpoint with no run identity: the plane contains every read to the
	// stage's own run, so it cannot be addressed without one.
	setPlaneEnv(t, "http://daemon.invalid:7777", "tok", "", noopPlaneGaggle)
	if err := recordPRRemediationNoop(l, "run-1"); !errors.Is(err, journalclient.ErrEndpointWithoutRun) {
		t.Fatalf("err = %v, want ErrEndpointWithoutRun", err)
	}

	// A run that is not the token's own is refused by the seam, by name.
	setPlaneEnv(t, "http://daemon.invalid:7777", "tok", "my-run", noopPlaneGaggle)
	err = recordPRRemediationNoop(l, "someone-elses-run")
	if err == nil || !strings.Contains(err.Error(), "someone-elses-run") {
		t.Fatalf("err = %v, want a refusal naming the foreign run", err)
	}

	// Off the plane, a run with no journal on this host is still the loud
	// failure it always was, rather than a silently skipped record.
	setPlaneEnv(t, "", "", "", "")
	if err := recordPRRemediationNoop(l, "never-existed"); err == nil {
		t.Fatal("a missing run journal was tolerated; the no-op record would be silently skipped")
	}
}

// seedNoopRemediationRun writes a finished pr-remediation journal — a rebase
// that attempted a head with a cause, and an implement that reported no-work —
// and mints the run's pod token. That is exactly the shape
// preparePRRemediationNoopUpdate reduces to a no-op signature.
func seedNoopRemediationRun(t *testing.T, plane *claimsPlane, gaggle, runID string) string {
	t.Helper()
	run, err := journal.Create(plane.layout.ForGaggle(gaggle).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "pr-remediation", Gaggle: gaggle,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{
			Type: journal.EventStageFinished, Stage: "rebase-pr", Status: string(apiv1.ResultSuccess),
			Outputs: map[string]any{"attemptedHeadSha": "head-a", "remediationCauses": "substantive"},
		},
		{Type: journal.EventStageFinished, Stage: "implement", Status: string(apiv1.ResultNoWork)},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	token, err := plane.registry.Mint(runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// TestRecordPRRemediationNoopResolvesItsClaimOverTheClaimsPlane is the C1 half.
//
// The writer has to know WHICH PR the finished run was remediating, and that
// answer lives in the claim ledger. Reading it with localscheduler.OpenClaimLedger
// over the instance's claims.json is what a pod cannot do; through the claims
// seam the same question is one round trip at the daemon, and the record that
// comes out is identical.
func TestRecordPRRemediationNoopResolvesItsClaimOverTheClaimsPlane(t *testing.T) {
	plane := newClaimsPlane(t)
	const runID = "run-remediation-plane"
	token := seedNoopRemediationRun(t, plane, noopPlaneGaggle, runID)

	claimKey := claimsclient.Key{
		Gaggle: noopPlaneGaggle, Provider: "github", ExternalID: pullRequestClaimKey(77),
	}
	ok, holder, err := plane.client(t, runID, token).ClaimScoped(
		t.Context(), claimKey, runID, "pr-remediation", time.Hour)
	if err != nil || !ok {
		t.Fatalf("claim PR 77: ok = %v, holder = %q, err = %v", ok, holder, err)
	}

	stampClaimsPlaneEnv(t, plane, runID, token)
	if err := recordPRRemediationNoop(plane.layout, runID); err != nil {
		t.Fatalf("recordPRRemediationNoop over the claims plane: %v", err)
	}

	record := remediationNoopStateRecord(t, plane.layout, remediationNoopKey(noopPlaneGaggle, 77))
	if record.Attempts != 1 || record.LastRunID != runID ||
		record.HeadSHA != "head-a" || record.Causes != "substantive" {
		t.Fatalf("record = %+v, want one no-op recorded for the plane-resolved PR claim", record)
	}

	// Idempotent: the same run recorded twice is still one attempt, exactly as
	// on the file backend.
	if err := recordPRRemediationNoop(plane.layout, runID); err != nil {
		t.Fatal(err)
	}
	if again := remediationNoopStateRecord(t, plane.layout, remediationNoopKey(noopPlaneGaggle, 77)); again != record {
		t.Fatalf("second record = %+v, want %+v", again, record)
	}
}

// TestRecordPRRemediationNoopSurfacesClaimsPlaneFailures is the claims half's
// fail-closed property: a ledger read that could not be served must stop the
// writer, not be read as "this run claimed no PR" — which would silently drop
// the no-op and let the lane re-attempt.
func TestRecordPRRemediationNoopSurfacesClaimsPlaneFailures(t *testing.T) {
	plane := newClaimsPlane(t)
	const runID = "run-remediation-refused"
	seedNoopRemediationRun(t, plane, noopPlaneGaggle, runID)

	// An endpoint with no bearer.
	t.Setenv(claimsclient.EnvEndpoint, plane.server.URL)
	t.Setenv(claimsclient.EnvToken, "")
	t.Setenv(claimsclient.EnvRunID, runID)
	if err := recordPRRemediationNoop(plane.layout, runID); !errors.Is(err, claimsclient.ErrEndpointWithoutToken) {
		t.Fatalf("err = %v, want ErrEndpointWithoutToken", err)
	}

	// An endpoint with no run identity.
	t.Setenv(claimsclient.EnvToken, "claims-token")
	t.Setenv(claimsclient.EnvRunID, "")
	if err := recordPRRemediationNoop(plane.layout, runID); !errors.Is(err, claimsclient.ErrEndpointWithoutRun) {
		t.Fatalf("err = %v, want ErrEndpointWithoutRun", err)
	}

	// A bearer the daemon will not honour: a transport-level refusal, still an
	// error rather than an empty claim list.
	t.Setenv(claimsclient.EnvToken, "not-a-minted-token")
	t.Setenv(claimsclient.EnvRunID, runID)
	if err := recordPRRemediationNoop(plane.layout, runID); err == nil {
		t.Fatal("a refused ledger read was treated as a run that claimed no PR")
	}

	// Nothing was recorded on the way through any of those.
	if record := remediationNoopStateRecord(t, plane.layout, remediationNoopKey(noopPlaneGaggle, 77)); !record.empty() {
		t.Fatalf("record = %+v, want nothing written by a refused ledger read", record)
	}
}

// TestRemediationNoopRecordRefusesUnreadableState is the last fail-open route
// closed. A record that is present but cannot be decoded — corrupt bytes, an
// unknown schema, or a document keyed to another PR — must be an error. Reading
// it as the zero record would be exactly the pod behaviour this issue removes,
// only triggered by corruption instead of by placement.
func TestRemediationNoopRecordRefusesUnreadableState(t *testing.T) {
	t.Setenv(stateclient.EnvEndpoint, "")
	l := layoutFor(initDemo(t))
	key := remediationNoopKey("", 77)
	path := filepath.Join(l.SchedulerDir(), remediationNoopStateKey(key))

	for name, body := range map[string]string{
		"corrupt bytes":  `{"schema":`,
		"unknown schema": `{"schema":"goobers.dev/pr-remediation-noop/v99","key":"/pr/77","record":{}}`,
		"foreign record": `{"schema":"` + remediationNoopSchema + `","key":"other/pr/9","record":{"attempts":2}}`,
		"not an object":  `["records"]`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			store, err := remediationNoopStore(l)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readRemediationNoopRecord(t.Context(), store, key); err == nil {
				t.Fatal("an unreadable record decoded as an empty guard")
			}
			if _, _, err := recordGatherPRContextDigestNoop(l, 77, remediationNoopSignature{HeadSHA: "h"}, "run-1", false); err == nil {
				t.Fatal("gather-pr-context's writer accepted an unreadable record")
			}
		})
	}
}

// TestLegacyRemediationNoopStateMigratesToKeyedFiles is the upgrade path, and
// it is a correctness requirement rather than tidiness: the aggregate file on a
// live instance holds the records that are actively suppressing re-attempts, so
// an instance that crossed this change without migrating would spend one full
// agentic remediation cycle per suppressed PR rediscovering that there is
// nothing to do.
func TestLegacyRemediationNoopStateMigratesToKeyedFiles(t *testing.T) {
	t.Setenv(stateclient.EnvEndpoint, "")
	l := layoutFor(initDemo(t))
	legacyPath := filepath.Join(l.SchedulerDir(), legacyRemediationNoopStateFile)

	suppressed := remediationNoopKey(noopPlaneGaggle, 77)
	parked := remediationNoopKey("", 78)
	preexisting := remediationNoopKey(noopPlaneGaggle, 79)

	// A record that already has a keyed value must NOT be overwritten: the
	// keyed value is by definition the newer of the two.
	store, err := remediationNoopStore(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := updateRemediationNoopState(t.Context(), store,
		preexisting, remediationNoopSignature{HeadSHA: "fresh"}, "run-fresh"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(legacyPath, []byte(`{"records":{
		"`+suppressed+`":{"headSha":"head-a","causes":"substantive","attempts":2,"lastRunId":"run-old"},
		"`+parked+`":{"headSha":"head-b","diffDigest":"sha256:d","attempts":1,"lastRunId":"run-older","parked":true},
		"`+preexisting+`":{"headSha":"stale","attempts":9,"lastRunId":"run-ancient"}
	}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := migrateLegacyRemediationNoopState(l); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the legacy aggregate survived the migration (err = %v)", err)
	}

	for key, want := range map[string]remediationNoopRecord{
		suppressed: {
			remediationNoopSignature: remediationNoopSignature{HeadSHA: "head-a", Causes: "substantive"},
			Attempts:                 2, LastRunID: "run-old",
		},
		parked: {
			remediationNoopSignature: remediationNoopSignature{HeadSHA: "head-b", DiffDigest: "sha256:d"},
			Attempts:                 1, LastRunID: "run-older", Parked: true,
		},
		preexisting: {
			remediationNoopSignature: remediationNoopSignature{HeadSHA: "fresh"},
			Attempts:                 1, LastRunID: "run-fresh",
		},
	} {
		got := remediationNoopStateRecord(t, l, key)
		if got != want {
			t.Fatalf("record for %q = %+v, want %+v", key, got, want)
		}
		if _, err := os.Stat(filepath.Join(l.SchedulerDir(), remediationNoopStateKey(key))); err != nil {
			t.Fatalf("no keyed file for %q: %v", key, err)
		}
	}

	// Idempotent, and a no-op once the aggregate is gone — the daemon runs it
	// on every start.
	if err := migrateLegacyRemediationNoopState(l); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if got := remediationNoopStateRecord(t, l, suppressed); got.Attempts != 2 {
		t.Fatalf("record after a second migration = %+v, want it untouched", got)
	}

	// And the migrated record is immediately live for the production readers:
	// a run that would have been the third attempt is at the limit, not at one.
	record, reset, err := recordGatherPRContextDigestNoop(
		l.ForGaggle(noopPlaneGaggle), 77,
		remediationNoopSignature{HeadSHA: "head-a", Causes: "substantive"}, "run-new", false)
	if err != nil || reset {
		t.Fatalf("record = %+v, reset = %v, err = %v", record, reset, err)
	}
	if record.Attempts < remediationNoopLimit {
		t.Fatalf("record = %+v, want the migrated streak to still be suppressing at limit %d",
			record, remediationNoopLimit)
	}
}
