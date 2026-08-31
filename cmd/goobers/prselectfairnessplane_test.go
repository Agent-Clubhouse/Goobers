package main

import (
	"encoding/json"
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

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// prselectfairnessplane_test.go is Goobers#3988's evidence: pr-select's
// fairness lease is one lease, wherever the stage runs.
//
// The lease (#1336's aging plus the one-hour starvation guard) was the last
// thing holding `pr-select` to executor.StageRequiresInstanceRoot, and it is
// load-bearing rather than a cache — an absent lease in a pod reads as a FRESH
// lease on every run, so every candidate's wait resets to zero, the guard
// never fires, and a single PR can monopolise the one pr-select slot
// undetected. These tests pin the four things that make plane admission a
// real fix rather than a relocation of the bug: the key addresses the
// production file, it rides the lock the claim transaction already holds, a
// pod and the daemon advance ONE lease, and every way of naming something
// else is refused server-side.

// prSelectPlaneRepo is the repository every test in this file leases against.
var prSelectPlaneRepo = providers.RepositoryRef{
	Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo",
}

// readPRSelectFairnessLease reads the lease straight off an instance's
// scheduler directory, deliberately WITHOUT the seam under test: an assertion
// that went back through openStageStateStore could not tell a lease that
// landed on the daemon from one that landed beside the pod.
func readPRSelectFairnessLease(root string) (prSelectFairnessFile, error) {
	path := filepath.Join(layoutFor(root).SchedulerDir(), prSelectFairnessFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return prSelectFairnessFile{}, nil
	}
	if err != nil {
		return prSelectFairnessFile{}, err
	}
	return decodePRSelectFairness(stateclient.Value{Data: data, ETag: stateclient.ETagFor(data)})
}

// TestPRSelectFairnessKeyAddressesTheProductionPath is the parity check that
// makes admitting this key safe at all.
//
// The lease is not a new file: it is the one #1336 introduced and the daemon
// has been reading and rewriting at <schedulerDir>/pr-select-fairness.json
// ever since. If the key resolved anywhere else, a pod-executed selection
// would age one lease while a runner-driven one aged another — which is the
// two-lease split the whole change exists to prevent — so the resolved path is
// asserted against the production layout byte for byte.
func TestPRSelectFairnessKeyAddressesTheProductionPath(t *testing.T) {
	if prSelectFairnessFileName != stateclient.KeyPRSelectFairness {
		t.Fatalf("file name %q != key %q", prSelectFairnessFileName, stateclient.KeyPRSelectFairness)
	}
	if !stateclient.ValidKey(stateclient.KeyPRSelectFairness) {
		t.Fatalf("key %q is outside the closed namespace", stateclient.KeyPRSelectFairness)
	}
	relative, err := stateclient.KeyRelativePath(stateclient.KeyPRSelectFairness)
	if err != nil {
		t.Fatal(err)
	}
	layout := instance.NewLayout(t.TempDir())
	got := filepath.Join(layout.SchedulerDir(), relative)
	want := filepath.Join(layout.SchedulerDir(), "pr-select-fairness.json")
	if got != want {
		t.Fatalf("key resolves to\n %s\nwant\n %s", got, want)
	}
	// One path element, so the route's structural pod-scope match holds and
	// the key can never acquire a second segment on the wire.
	if relative != filepath.Base(relative) {
		t.Fatalf("key resolves to a multi-element path %q", relative)
	}
}

// TestPRSelectFairnessKeyRidesTheClaimsLock is finding 002's "same lock the
// key already used" rule, applied to this key and pinned.
//
// claims.lock is not an arbitrary choice here and must not drift to a lock of
// its own: the lease is observed and rewritten INSIDE pr-select's claim
// transaction (observePRSelectEligibility), so a candidate's wait and the
// claim that ends it move as one step. Put the lease on a second lock and a
// concurrent run could observe a PR as unclaimed-and-aging while another run
// is halfway through claiming it.
func TestPRSelectFairnessKeyRidesTheClaimsLock(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := schedulerStateLock(layout)

	if err := lock(stateclient.KeyPRSelectFairness, claimLockOperationPRSelectFairnessObserve, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(layout.SchedulerDir(), claimLockFileName)); err != nil {
		t.Fatalf("the fairness lease did not take %s: %v", claimLockFileName, err)
	}
	// No private lock file: a second lock would put the lease outside the
	// claim transaction's atomicity domain, which is the whole of its mutual
	// exclusion.
	entries, err := os.ReadDir(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".lock") && entry.Name() != claimLockFileName {
			t.Fatalf("the fairness lease created a second lock file %q; it must share claims.lock", entry.Name())
		}
	}
}

// TestDaemonAndPodAdvanceOnePRSelectLease is the interleaving case #3988 asks
// for: a stage POD and the DAEMON advance the SAME lease concurrently.
//
//   - the daemon-driven writer runs in the daemon's own process through the
//     file backend under claims.lock — what a mode-1/mode-2 pr-select does;
//   - the pod-driven writer runs through the plane, where the daemon takes
//     that same claims.lock on the caller's behalf — what a mode-3 pr-select
//     does.
//
// If the two ran in separate atomicity domains, entries would be lost: each
// would read the same lease, add its own candidates, and one would overwrite
// the other — the silent two-lease split that would let a starving PR's wait
// vanish. The assertion is exact: every candidate either writer added must
// survive, with the EARLIEST eligibleSince of the two, because that is the
// value the aging boost and the starvation guard are computed from.
func TestDaemonAndPodAdvanceOnePRSelectLease(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-pod")

	pod := plane.client(t, "goobers", token)
	daemon := plane.daemonStore(t)

	const perWriter = 12
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	// Disjoint PR-number ranges, so a lost write is unambiguous rather than a
	// coincidence of two writers computing the same entry.
	advance := func(store stateclient.Store, first int) error {
		for i := 0; i < perWriter; i++ {
			number := first + i
			err := store.Update(t.Context(), stateclient.KeyPRSelectFairness, claimLockOperationPRSelectFairnessObserve,
				func(value stateclient.Value) ([]byte, bool, error) {
					state, decodeErr := decodePRSelectFairness(value)
					if decodeErr != nil {
						return nil, false, decodeErr
					}
					for _, entry := range state.Candidates {
						if entry.Number == number {
							return nil, false, nil
						}
					}
					state.Candidates = append(state.Candidates, prSelectFairnessEntry{
						Gaggle:        "goobers",
						Repository:    prSelectFairnessScope(prSelectPlaneRepo),
						Number:        number,
						HeadSHA:       "head-" + strconv.Itoa(number),
						EligibleSince: base.Add(time.Duration(number) * time.Minute),
						LastObserved:  base.Add(time.Duration(number) * time.Minute),
					})
					data, encodeErr := encodePRSelectFairness(state)
					return data, true, encodeErr
				})
			if err != nil {
				return err
			}
		}
		return nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	firsts := []int{100, 200}
	for index, store := range []stateclient.Store{pod, daemon} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[index] = advance(store, firsts[index])
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	value, err := daemon.Get(t.Context(), stateclient.KeyPRSelectFairness)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodePRSelectFairness(value)
	if err != nil {
		t.Fatalf("the final lease does not decode: %v", err)
	}
	if len(state.Candidates) != 2*perWriter {
		t.Fatalf("lease holds %d candidates, want %d — %d were lost, so the pod and the daemon are NOT in one atomicity domain",
			len(state.Candidates), 2*perWriter, 2*perWriter-len(state.Candidates))
	}
	byNumber := make(map[int]prSelectFairnessEntry, len(state.Candidates))
	for _, entry := range state.Candidates {
		byNumber[entry.Number] = entry
	}
	for _, first := range firsts {
		for i := 0; i < perWriter; i++ {
			number := first + i
			entry, ok := byNumber[number]
			if !ok {
				t.Fatalf("PR #%d's wait vanished from the shared lease", number)
			}
			if want := base.Add(time.Duration(number) * time.Minute); !entry.EligibleSince.Equal(want) {
				t.Fatalf("PR #%d eligibleSince = %s, want %s", number, entry.EligibleSince, want)
			}
		}
	}

	// The lease advanced on the DAEMON's volume, at the production path — the
	// far-side evidence #3988 asks for, in miniature.
	if _, err := os.Stat(filepath.Join(plane.layout.SchedulerDir(), prSelectFairnessFileName)); err != nil {
		t.Fatalf("the lease did not land on the daemon's scheduler directory: %v", err)
	}
}

// TestPRSelectFairnessAuthorizationIsGaggleScoped is the plane's containment,
// asserted for this key: a pod's bearer admits it to the gaggle its own run
// belongs to and to nothing else.
//
// The lease is gaggle-agnostic in its NAME (unlike the backlog-health cursor),
// so the route's own {gaggle} segment is the whole of its scope — which makes
// the run-belongs-to-gaggle check the only thing standing between a pod
// contained to gaggle A and gaggle B's fairness lease. A cross-gaggle write
// here would reset another gaggle's waits to zero, which is exactly the
// starvation the guard exists to detect.
func TestPRSelectFairnessAuthorizationIsGaggleScoped(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-own")
	// A run that exists on the daemon under a DIFFERENT gaggle, so the only
	// thing separating the two calls is the containment check.
	plane.admitRun(t, "other", "run-foreign")

	own := plane.client(t, "goobers", token)
	if _, err := own.Put(t.Context(), stateclient.KeyPRSelectFairness, []byte(`{"candidates":[]}`), ""); err != nil {
		t.Fatalf("own-gaggle write: %v", err)
	}
	if _, err := own.Get(t.Context(), stateclient.KeyPRSelectFairness); err != nil {
		t.Fatalf("own-gaggle read: %v", err)
	}

	var planeErr *stateclient.Error
	for _, foreign := range []string{"other", "goobers-staging", "goober"} {
		// The SAME bearer, aimed at another gaggle's route.
		crossed := plane.client(t, foreign, token)
		if _, err := crossed.Get(t.Context(), stateclient.KeyPRSelectFairness); !errors.As(err, &planeErr) || planeErr.Status != http.StatusForbidden {
			t.Fatalf("read of gaggle %q err = %v, want a typed 403", foreign, err)
		}
		if _, err := crossed.Put(t.Context(), stateclient.KeyPRSelectFairness, []byte(`{"candidates":[]}`), ""); !errors.As(err, &planeErr) || planeErr.Status != http.StatusForbidden {
			t.Fatalf("write of gaggle %q err = %v, want a typed 403", foreign, err)
		}
	}
}

// TestStatePlaneRefusesPRSelectFairnessKeyVariants keeps the admission of one
// fixed key from becoming an arbitrary-read primitive. The refusal must not
// depend on a well-behaved client, so every variant is driven at the route.
func TestStatePlaneRefusesPRSelectFairnessKeyVariants(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-1")

	const secretBody = `{"claims":"secret"}`
	for _, secret := range []string{
		filepath.Join(plane.layout.SchedulerDir(), "claims.json"),
		filepath.Join(plane.layout.SchedulerDir(), "pr-select-fairness.json.bak"),
	} {
		if err := os.WriteFile(secret, []byte(secretBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, key := range []string{
		// Traversals, escaped and plain.
		"%2e%2e%2fclaims.json",
		".." + string(filepath.Separator) + "pr-select-fairness.json",
		"pr-select-fairness.json/../claims.json",
		// Near-miss spellings of the admitted key.
		"pr-select-fairness.json.bak",
		"pr-select-fairness.json.tmp",
		"pr-select-fairness",
		"PR-Select-Fairness.json",
		"pr-select-fairness.JSON",
		" pr-select-fairness.json",
		"pr-select-fairness.json ",
		"./pr-select-fairness.json",
		// Files in the same directory the key resolves into.
		"claims.json",
		"claims.lock",
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			plane.server.URL+"/api/v1/gaggles/goobers/state/"+key, nil)
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
		// Refused as a key (400), as a route that does not exist (404), or as
		// a scope violation (403) — never resolved into a value.
		switch response.StatusCode {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusForbidden:
		default:
			t.Fatalf("key %q status = %d, want it refused", key, response.StatusCode)
		}
		if strings.Contains(string(body), `"claims":"secret"`) {
			t.Fatalf("key %q served a file outside the closed namespace", key)
		}
	}

	// The same refusal in the client, so a variant cannot even leave the
	// stage: ValidKey is the guard both backends apply before a key becomes a
	// path or a URL.
	for _, key := range []string{
		"pr-select-fairness.json.bak",
		"../pr-select-fairness.json",
		"PR-Select-Fairness.json",
	} {
		if stateclient.ValidKey(key) {
			t.Fatalf("ValidKey admitted %q", key)
		}
		if _, err := stateclient.KeyRelativePath(key); !errors.Is(err, stateclient.ErrInvalidKey) {
			t.Fatalf("KeyRelativePath(%q) err = %v, want ErrInvalidKey", key, err)
		}
	}
}

// prSelectPlaneFixture builds a pod-side instance root and a fake provider
// holding two open, eligible PRs — enough for a real `goobers pr-select` to
// run end to end and lease both candidates.
func prSelectPlaneFixture(t *testing.T, runID string, numbers ...int) string {
	t.Helper()
	root := initDemo(t)
	server := newFakeGitHubServer(t, prSelectPlaneRepo.Owner, prSelectPlaneRepo.Name)
	for _, number := range numbers {
		name := "pr-" + strconv.Itoa(number)
		server.addIssue(number, name)
		server.addOpenPR(number, "goobers/implementation/"+name, "main",
			"head-"+name, "base", false, nil, nil)
	}
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	return root
}

// TestPRSelectWritesItsLeaseThroughToTheDaemon is the end-to-end proof that
// closes #3988: with the environment a stage pod carries, a REAL `goobers
// pr-select` lands its fairness lease in the DAEMON's scheduler directory and
// writes nothing beside the pod.
//
// This is the behaviour whose absence kept pr-select on
// executor.StageRequiresInstanceRoot: an un-planed pod has no instance root,
// so the lease resolved under "." and vanished with the container — every run
// read a fresh lease, nothing ever aged, and the one-hour starvation guard
// could never fire.
func TestPRSelectWritesItsLeaseThroughToTheDaemon(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-select")
	root := prSelectPlaneFixture(t, "run-select", 10, 20)
	stampStatePlaneEnv(t, plane, "goobers", token)

	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "selected PR #") {
		t.Fatalf("pr-select selected nothing: stdout = %q", stdout)
	}

	// The DAEMON's file, at the production path — not a pod-local one.
	daemonPath := filepath.Join(plane.layout.SchedulerDir(), prSelectFairnessFileName)
	data, err := os.ReadFile(daemonPath)
	if err != nil {
		t.Fatalf("the lease did not land on the daemon: %v", err)
	}
	var persisted prSelectFairnessFile
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("the daemon's lease does not parse: %v", err)
	}
	if len(persisted.Candidates) == 0 {
		t.Fatalf("the daemon's lease is empty: %s", data)
	}
	for _, entry := range persisted.Candidates {
		if entry.Gaggle != "goobers" || entry.Repository != prSelectFairnessScope(prSelectPlaneRepo) {
			t.Fatalf("the daemon's lease is not this run's: %+v", entry)
		}
	}

	// Nothing beside the pod. The pod's own tree is the instance root the
	// fixture builds, which is what a mis-resolved lease would have used.
	podPath := filepath.Join(layoutFor(root).SchedulerDir(), prSelectFairnessFileName)
	if _, err := os.Stat(podPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a lease was written beside the pod at %s (err = %v); the plane write fell through", podPath, err)
	}
}

// TestPodSelectionSeesTheDaemonsAccumulatedWait is the semantic half of "one
// lease": it is not enough that the bytes land on the daemon, the pod's
// SELECTION must be computed from the wait the daemon already recorded.
//
// A daemon-driven run seeds a candidate that has been eligible for well over
// the one-hour starvation limit. A pod-driven selection then runs against the
// plane with a fresh, just-eligible PR also on offer. If the pod read its own
// empty lease it would see two equally-fresh candidates and pick the
// lower-numbered one; because it reads the daemon's, the starved PR is
// guarded and wins outright.
func TestPodSelectionSeesTheDaemonsAccumulatedWait(t *testing.T) {
	const (
		starvedNumber = 900
		freshNumber   = 10
	)
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-select")
	root := prSelectPlaneFixture(t, "run-select", freshNumber, starvedNumber)

	// The DAEMON's own writer, in the daemon's process, under claims.lock.
	starvedSince := time.Now().UTC().Add(-3 * time.Hour)
	daemon := plane.daemonStore(t)
	seeded, err := encodePRSelectFairness(prSelectFairnessFile{Candidates: []prSelectFairnessEntry{{
		Gaggle:        "goobers",
		Repository:    prSelectFairnessScope(prSelectPlaneRepo),
		Number:        starvedNumber,
		HeadSHA:       "head-pr-" + strconv.Itoa(starvedNumber),
		EligibleSince: starvedSince,
		LastObserved:  starvedSince,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.Put(t.Context(), stateclient.KeyPRSelectFairness, seeded, ""); err != nil {
		t.Fatal(err)
	}

	stampStatePlaneEnv(t, plane, "goobers", token)
	t.Chdir(t.TempDir())
	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "selected PR #"+strconv.Itoa(starvedNumber)) {
		t.Fatalf("the pod selected from a FRESH lease (stdout = %q); it must see the daemon's %s wait for PR #%d",
			stdout, time.Since(starvedSince).Truncate(time.Minute), starvedNumber)
	}

	// The pod's selection also RETIRED the winner's entry on the daemon —
	// clearPRSelectEligibilityWait over the same plane — so the next cycle
	// does not re-pay an aging boost that has already been spent.
	value, err := daemon.Get(t.Context(), stateclient.KeyPRSelectFairness)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodePRSelectFairness(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range state.Candidates {
		if entry.Number == starvedNumber {
			t.Fatalf("the selected PR's wait was not cleared on the daemon: %+v", entry)
		}
	}
}

// TestPRSelectFailsClosedOnAPartialPlaneConfig is the refusal that keeps a
// half-configured pod from doing the un-planed thing quietly. An endpoint
// without a bearer (or without a gaggle) must abort the stage, not fall back
// to a fairness lease the pod does not have and nothing will ever read —
// which is the fresh-lease silent starvation this change exists to prevent.
func TestPRSelectFailsClosedOnAPartialPlaneConfig(t *testing.T) {
	cases := []struct {
		name  string
		token string
		// gaggle is stamped over the fixture's own GOOBERS_GAGGLE.
		gaggle string
		want   error
	}{
		{name: "endpoint without a bearer", gaggle: "goobers", want: stateclient.ErrEndpointWithoutToken},
		{name: "endpoint without a gaggle", token: "a-token", gaggle: "", want: stateclient.ErrEndpointWithoutGaggle},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			plane := newStatePlane(t)
			root := prSelectPlaneFixture(t, "run-partial", 10, 20)
			t.Setenv(stateclient.EnvEndpoint, plane.server.URL)
			t.Setenv(stateclient.EnvToken, testCase.token)
			t.Setenv(stateclient.EnvGaggle, testCase.gaggle)

			t.Chdir(t.TempDir())
			code, stdout, stderr := runArgs(t, "pr-select", root)
			if code == 0 {
				t.Fatalf("a partial plane config succeeded: stdout = %q, stderr = %q", stdout, stderr)
			}
			// Nothing written locally, and nothing written on the daemon: a
			// refused stage leases nothing anywhere.
			for _, path := range []string{
				filepath.Join(layoutFor(root).SchedulerDir(), prSelectFairnessFileName),
				filepath.Join(plane.layout.SchedulerDir(), prSelectFairnessFileName),
			} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("the refused stage still wrote %s (err = %v)", path, err)
				}
			}

			// And the seam itself names the refusal, so the failure is the
			// fail-closed one rather than an incidental provider error.
			if _, err := openPRSelectFairnessStore(layoutFor(root)); !errors.Is(err, testCase.want) {
				t.Fatalf("openPRSelectFairnessStore err = %v, want %v", err, testCase.want)
			}
			if _, err := openHeldPRSelectFairnessStore(layoutFor(root)); !errors.Is(err, testCase.want) {
				t.Fatalf("openHeldPRSelectFairnessStore err = %v, want %v", err, testCase.want)
			}
		})
	}
}

// TestPRSelectLocalBehaviourIsUnchanged is the other half of the contract: a
// type-1/type-2 instance — no plane in its environment — keeps the file it
// had, at the path it had, with the bytes it had.
//
// The encoding is asserted byte for byte against the pre-plane writer's
// rendering (MarshalIndent with two spaces, gaggle/repository/number order, a
// trailing newline), because an instance that switches between the two
// backends must produce identical bytes or the content-digest ETag would
// differ across paths and every cross-backend compare-and-swap would refuse.
func TestPRSelectLocalBehaviourIsUnchanged(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	if statePlaneSelected() {
		t.Fatal("the local case must not have a plane in its environment")
	}

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	eligible := []providers.PullRequestSummary{
		{Number: 20, HeadSHA: "head-20"},
		{Number: 10, HeadSHA: "head-10"},
	}
	if _, err := observePRSelectEligibility(root, prSelectPlaneRepo, eligible, eligible, prSelectCompleteSnapshot, now); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(layoutFor(root).SchedulerDir(), "pr-select-fairness.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the lease is not at its historical path: %v", err)
	}
	want, err := json.MarshalIndent(prSelectFairnessFile{Candidates: []prSelectFairnessEntry{
		{Gaggle: "goobers", Repository: "your-org/your-repo", Number: 10, HeadSHA: "head-10", EligibleSince: now, LastObserved: now},
		{Gaggle: "goobers", Repository: "your-org/your-repo", Number: 20, HeadSHA: "head-20", EligibleSince: now, LastObserved: now},
	}}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if string(data) != string(want) {
		t.Fatalf("lease bytes changed:\n got %s\nwant %s", data, want)
	}

	// The aging the lease exists for still accrues locally, and the clear
	// still retires the winner.
	later := now.Add(2 * prSelectAgingInterval)
	observation, err := observePRSelectEligibility(root, prSelectPlaneRepo, eligible, eligible, prSelectCompleteSnapshot, later)
	if err != nil {
		t.Fatal(err)
	}
	_, priorities, _ := rankEligiblePullRequests(observation.UnclaimedEligible, nil, observation.EligibleSince, later)
	if got := priorities[10].AgingBoost; got != 2 {
		t.Fatalf("aging boost = %d, want 2 — the lease stopped accumulating locally", got)
	}
	if err := clearPRSelectEligibilityWait(root, prSelectPlaneRepo, providers.PullRequestSummary{Number: 10}); err != nil {
		t.Fatal(err)
	}
	state, err := readPRSelectFairnessLease(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Candidates) != 1 || state.Candidates[0].Number != 20 {
		t.Fatalf("lease after clear = %+v, want only PR #20", state.Candidates)
	}
}

// TestPRSelectNoLongerRequiresTheInstanceRoot is the removal itself, asserted
// where the engine reads it: with the lease on the plane, nothing in the
// stage's path holds a file under the daemon's instance root, so
// dispatchRemoteTask must stop refusing it before a pod is created.
func TestPRSelectNoLongerRequiresTheInstanceRoot(t *testing.T) {
	if executor.StageRequiresInstanceRoot([]string{"goobers", "pr-select"}, "") {
		t.Fatal("pr-select is still refused; the fairness lease is on the scheduler-state plane now")
	}
	if executor.StageRequiresInstanceRoot([]string{"goobers", "pr-select"}, "shell") {
		t.Fatal("pr-select with an explicit shell kind is still refused")
	}
}
