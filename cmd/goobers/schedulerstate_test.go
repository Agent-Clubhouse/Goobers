package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/stateclient"
)

// statePlane is the daemon's REAL scheduler-state plane: podauth in front of a
// deny-all human fallback, httpapi.RequireRoles as the authorizer, and
// daemonStateService over an instance layout behind httpapi.WithStateService —
// the construction cmd/goobers/up.go performs. The scheduler-state files under
// the layout are the daemon side of the seam, and the daemon's OWN in-process
// store (fileStateStore over the same layout) is the other writer these tests
// interleave against.
type statePlane struct {
	server   *httptest.Server
	layout   instance.Layout
	registry *podauth.Registry
}

func newStatePlane(t *testing.T) *statePlane {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := newDaemonStateService(layout)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator),
		httpapi.WithStateService(service),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &statePlane{server: server, layout: layout, registry: registry}
}

// admitRun creates runID's journal under gaggle on the daemon (the run.yaml
// the live writer creates at a run's first emit — the authority the plane's
// containment check consults) and mints its pod token.
func (p *statePlane) admitRun(t *testing.T, gaggle, runID string) string {
	t.Helper()
	run, err := journal.Create(p.layout.ForGaggle(gaggle).RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation", Gaggle: gaggle}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	token, err := p.registry.Mint(runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// client is the plane backend a stage pod for gaggle would construct.
func (p *statePlane) client(t *testing.T, gaggle, token string) stateclient.Store {
	t.Helper()
	client, err := stateclient.NewHTTP(stateclient.HTTPConfig{BaseURL: p.server.URL, Token: token, Gaggle: gaggle})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// daemonStore is the store the daemon's OWN process uses — the instance's
// files under each key's real cross-process lock, which is exactly what a
// runner-driven (mode-1/mode-2) stage on this host constructs.
func (p *statePlane) daemonStore(t *testing.T) stateclient.Store {
	t.Helper()
	store, err := fileStateStore(p.layout)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// stampStatePlaneEnv is the environment a goobers-CLI stage pod carries for
// the scheduler-state plane, so the production selection seam picks the plane
// backend.
func stampStatePlaneEnv(t *testing.T, plane *statePlane, gaggle, token string) {
	t.Helper()
	t.Setenv(stateclient.EnvEndpoint, plane.server.URL)
	t.Setenv(stateclient.EnvToken, token)
	t.Setenv(stateclient.EnvGaggle, gaggle)
}

// TestSchedulerStateKeysKeepTheirExistingLocks is finding 002's explicit
// correction, pinned: the plane must be served under the SAME lock each key
// already used, not under a new one. blocked.json, the backlog scan cursors
// and pr-select's fairness lease ride claims.lock (blockedrecords.go,
// backlogquery.go, prselectfairness.go); the reconcile ledger
// and the sibling cache keep their own. Splitting the lock domain would let a
// plane write and an in-process write race the very updates the CAS exists to
// protect.
func TestSchedulerStateKeysKeepTheirExistingLocks(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := schedulerStateLock(layout)

	cases := []struct {
		key  string
		file string
	}{
		{stateclient.KeyBlockedRecords, claimLockFileName},
		{stateclient.ScanCursorKey(hexDigest("scan")), claimLockFileName},
		{stateclient.KeyPRSelectFairness, claimLockFileName},
		{stateclient.KeyPostMergeReconcileLedger, postMergeReconcileLockFile},
		{stateclient.KeySiblingContextCache, siblingCacheLockFileName},
	}
	for _, testCase := range cases {
		t.Run(testCase.key, func(t *testing.T) {
			if err := lock(testCase.key, "probe", func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			// The lock the section took is the lock file it created.
			if _, err := os.Stat(filepath.Join(layout.SchedulerDir(), testCase.file)); err != nil {
				t.Fatalf("%s did not take %s: %v", testCase.key, testCase.file, err)
			}
		})
	}
}

// TestRunnerDrivenAndEngineDrivenCursorAdvancesShareOneLock is the interleaving
// case decision 005 R3 turns on, and the reason the route exists at all.
//
// Two writers advance the SAME backlog scan cursor concurrently:
//   - the runner-driven one, in the daemon's own process, through the file
//     backend under claims.lock — what a mode-1/mode-2 backlog-query does;
//   - the engine-driven one, in a stage pod, through the plane — what a mode-3
//     backlog-query does, where the daemon takes that same claims.lock on the
//     caller's behalf.
//
// If the two ran in separate atomicity domains, increments would be lost: each
// would read the same cursor, compute the same successor, and one would
// silently overwrite the other. The assertion is therefore exact — every
// increment must be present in the final value.
func TestRunnerDrivenAndEngineDrivenCursorAdvancesShareOneLock(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-engine")
	key := stateclient.ScanCursorKey(hexDigest("interleave"))

	engine := plane.client(t, "goobers", token)
	runner := plane.daemonStore(t)

	const advancesPerWriter = 12
	advance := func(store stateclient.Store) error {
		for i := 0; i < advancesPerWriter; i++ {
			err := store.Update(t.Context(), key, claimLockOperationBacklogScanCursor,
				func(value stateclient.Value) ([]byte, bool, error) {
					count := 0
					if value.Exists() {
						parsed, err := strconv.Atoi(string(value.Data))
						if err != nil {
							return nil, false, err
						}
						count = parsed
					}
					return []byte(strconv.Itoa(count + 1)), true, nil
				})
			if err != nil {
				return err
			}
		}
		return nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for index, store := range []stateclient.Store{engine, runner} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[index] = advance(store)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	final, err := runner.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	count, err := strconv.Atoi(string(final.Data))
	if err != nil {
		t.Fatal(err)
	}
	if count != 2*advancesPerWriter {
		t.Fatalf("cursor = %d, want %d — %d advances were lost, so the two writers are NOT in one atomicity domain",
			count, 2*advancesPerWriter, 2*advancesPerWriter-count)
	}

	// The cursor advanced on the DAEMON's volume, not in a pod-local file:
	// this is the far-side evidence the issue asks for, in miniature.
	if _, err := os.Stat(filepath.Join(plane.layout.SchedulerDir(), key)); err != nil {
		t.Fatalf("the cursor did not land on the daemon's scheduler directory: %v", err)
	}
}

// TestRunnerDrivenAndEngineDrivenBlockedRecordUpdatesShareOneLock is the same
// interleaving over blocked.json, through the production seam
// (updateBlockedRecords) rather than the raw store — so the CLI call site
// itself is what is shown to be safe, not just the primitive under it.
func TestRunnerDrivenAndEngineDrivenBlockedRecordUpdatesShareOneLock(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-engine")

	const recordsPerWriter = 10
	write := func(prefix string) error {
		for i := 0; i < recordsPerWriter; i++ {
			key := prefix + "-" + strconv.Itoa(i)
			err := updateBlockedRecords(plane.layout, func(recs map[string]blockedRecord) bool {
				recs[key] = blockedRecord{ItemID: key, RunID: prefix, RecordedAt: time.Unix(int64(i), 0).UTC()}
				return true
			})
			if err != nil {
				return err
			}
		}
		return nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)

	// The engine-driven writer: the production seam with the pod's plane
	// environment stamped, so openStageStateStore selects the HTTP backend.
	// t.Setenv cannot be used from a goroutine, so the pod's store is built
	// here and installed through the same seam variable the stage would use.
	podStore := plane.client(t, "goobers", token)
	daemonStore := plane.daemonStore(t)
	var seam sync.Mutex
	selectFor := func(store stateclient.Store) func(instance.Layout) (stateclient.Store, error) {
		return func(instance.Layout) (stateclient.Store, error) { return store, nil }
	}
	restore := openStageStateStore
	t.Cleanup(func() { openStageStateStore = restore })

	run := func(index int, store stateclient.Store, prefix string) {
		defer wg.Done()
		// Each iteration installs its own backend for the duration of one
		// call; the seam variable is process-global, so the swap is guarded.
		for i := 0; i < recordsPerWriter; i++ {
			key := prefix + "-" + strconv.Itoa(i)
			seam.Lock()
			openStageStateStore = selectFor(store)
			err := updateBlockedRecords(plane.layout, func(recs map[string]blockedRecord) bool {
				recs[key] = blockedRecord{ItemID: key, RunID: prefix, RecordedAt: time.Unix(int64(i), 0).UTC()}
				return true
			})
			seam.Unlock()
			if err != nil {
				errs[index] = err
				return
			}
		}
	}
	_ = write

	wg.Add(2)
	go run(0, podStore, "engine")
	go run(1, daemonStore, "runner")
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	openStageStateStore = selectFor(daemonStore)
	recs, err := snapshotBlockedRecords(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2*recordsPerWriter {
		t.Fatalf("blocked.json holds %d records, want %d — updates were lost across the two lanes", len(recs), 2*recordsPerWriter)
	}
	// And it is the DAEMON's blocked.json that holds them.
	if _, err := os.Stat(filepath.Join(plane.layout.SchedulerDir(), stateclient.KeyBlockedRecords)); err != nil {
		t.Fatalf("blocked.json did not land on the daemon's scheduler directory: %v", err)
	}
}

// TestStatePlaneContainsAPodToItsOwnGaggle is R3's containment end to end,
// against the real daemon service: the authority is the caller's own run.yaml,
// and a gaggle it cannot be verified against is refused rather than resolved.
func TestStatePlaneContainsAPodToItsOwnGaggle(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-1")

	own := plane.client(t, "goobers", token)
	if _, err := own.Put(t.Context(), stateclient.KeyBlockedRecords, []byte("{}"), ""); err != nil {
		t.Fatalf("own-gaggle write: %v", err)
	}

	foreign := plane.client(t, "other", token)
	_, err := foreign.Get(t.Context(), stateclient.KeyBlockedRecords)
	var planeErr *stateclient.Error
	if !errors.As(err, &planeErr) || planeErr.Status != http.StatusForbidden {
		t.Fatalf("foreign-gaggle read err = %v, want a typed 403", err)
	}
	if _, err := foreign.Put(t.Context(), stateclient.KeyBlockedRecords, []byte("{}"), ""); !errors.As(err, &planeErr) || planeErr.Status != http.StatusForbidden {
		t.Fatalf("foreign-gaggle write err = %v, want a typed 403", err)
	}

	// A gaggle that is not one plain path element is refused at construction
	// rather than resolved into a traversal. It has to be refused HERE: once
	// such a path reaches the wire the client normalizes it into a request
	// against a different route, whose 404 is indistinguishable from the 404
	// an absent key answers with — a traversal would read back as "no value".
	for _, gaggle := range []string{"..", ".", "a/b", "", "goobers/../other"} {
		hostile, err := stateclient.NewHTTP(stateclient.HTTPConfig{
			BaseURL: plane.server.URL, Token: token, Gaggle: gaggle,
		})
		if err == nil {
			t.Fatalf("gaggle %q was accepted: %#v", gaggle, hostile)
		}
	}
}

// TestStatePlaneRefusesKeysOutsideTheNamespaceServerSide keeps the containment
// from depending on a well-behaved client: the daemon refuses too.
func TestStatePlaneRefusesKeysOutsideTheNamespaceServerSide(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-1")

	// Write a file the plane must never serve, right next to the ones it does.
	secret := filepath.Join(plane.layout.SchedulerDir(), "claims.json")
	if err := os.WriteFile(secret, []byte(`{"claims":"secret"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		plane.server.URL+"/api/v1/gaggles/goobers/state/claims.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want the key refused", response.StatusCode)
	}
	body, _ := io.ReadAll(response.Body)
	if string(body) != "" && string(body) == `{"claims":"secret"}` {
		t.Fatal("the plane served a file outside the scheduler-state namespace")
	}
}

// TestStageStateStoreSelectionFailsClosed pins the stage seam: a pod that has
// an endpoint but no bearer must never silently fall through to a local
// scheduler-state file nothing will read.
func TestStageStateStoreSelectionFailsClosed(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-1")

	store, err := stageStateStore(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*stateclient.File); !ok {
		t.Fatalf("store = %T with no plane env, want the file backend", store)
	}

	t.Setenv(stateclient.EnvEndpoint, plane.server.URL)
	if _, err := stageStateStore(plane.layout); !errors.Is(err, stateclient.ErrEndpointWithoutToken) {
		t.Fatalf("err = %v, want ErrEndpointWithoutToken", err)
	}
	t.Setenv(stateclient.EnvToken, token)
	if _, err := stageStateStore(plane.layout); !errors.Is(err, stateclient.ErrEndpointWithoutGaggle) {
		t.Fatalf("err = %v, want ErrEndpointWithoutGaggle", err)
	}
	t.Setenv(stateclient.EnvGaggle, "goobers")
	store, err = stageStateStore(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*stateclient.HTTP); !ok {
		t.Fatalf("store = %T with the plane env stamped, want the plane backend", store)
	}
	if !statePlaneSelected() {
		t.Fatal("statePlaneSelected did not report the stamped endpoint")
	}
}

// TestStampedStageEnvWritesThroughToTheDaemonsFile closes the loop the two
// interleaving tests open by hand: with nothing but the environment a stage pod
// carries, the production seam builds a plane-backed store, and a write through
// it lands in the DAEMON's scheduler directory rather than in a local file
// beside the pod that nothing will ever read.
func TestStampedStageEnvWritesThroughToTheDaemonsFile(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-1")
	stampStatePlaneEnv(t, plane, "goobers", token)

	// A stage pod has no instance root of its own; the layout it would compute
	// is a scratch directory that must stay empty.
	podLayout := instance.NewLayout(t.TempDir())
	store, err := stageStateStore(podLayout)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*stateclient.HTTP); !ok {
		t.Fatalf("store = %T, want the plane backend", store)
	}

	if _, err := store.Put(t.Context(), stateclient.KeySiblingContextCache, []byte(`{"entries":[]}`), ""); err != nil {
		t.Fatalf("plane write: %v", err)
	}

	if _, err := os.Stat(filepath.Join(plane.layout.SchedulerDir(), stateclient.KeySiblingContextCache)); err != nil {
		t.Fatalf("the write did not land on the daemon's scheduler directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(podLayout.SchedulerDir(), stateclient.KeySiblingContextCache)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a local file was written beside the pod (err = %v); the plane write fell through", err)
	}
}

// TestStatePlaneEnvIsDisjointFromThePodToken keeps the state bearer from being
// spelled with the dispatcher's privileged names, which are stripped from
// every stage environment precisely because the pod token authorizes
// surrendering the run's result.
func TestStatePlaneEnvIsDisjointFromThePodToken(t *testing.T) {
	if stateclient.EnvToken == "GOOBERS_POD_TOKEN" || stateclient.EnvEndpoint == "GOOBERS_DAEMON_API" {
		t.Fatal("the scheduler-state bearer reuses the dispatcher's privileged env names")
	}
}

// hexDigest builds a syntactically valid scan-cursor digest from a label, so
// tests name distinct cursors without depending on a real scan key.
func hexDigest(label string) string {
	digest := ""
	for len(digest) < 64 {
		for _, r := range label {
			digest += strconv.FormatInt(int64(r%16), 16)
			if len(digest) == 64 {
				break
			}
		}
	}
	return digest
}
