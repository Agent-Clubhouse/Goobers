package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// backlogHealthPlaneKey is the state key for the scan every test in this file
// drives, and backlogHealthPlaneRepo/Label are its coordinates. Kept together
// so the key and the path assertions cannot drift apart by editing one.
var backlogHealthPlaneRepo = providers.RepositoryRef{
	Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo",
}

func backlogHealthPlaneKey(gaggle string) string {
	return backlogHealthCursorKey(gaggle, backlogHealthPlaneRepo, providers.LabelReady)
}

// TestBacklogHealthCursorKeyAddressesTheProductionPath is the parity check that
// makes admitting this key to the C2 namespace safe at all (#3948).
//
// The ready-transition ledger is not a new file: it is the one #3392 introduced
// and the daemon has been reading and writing at
// <schedulerDir>/backlog-health/<gaggle>__<provider>__<repo>__<label>.json ever
// since. If the key resolved anywhere else, a pod-executed cycle would advance
// one ledger while a runner-driven one advanced another, and the whole point of
// the durable cursor — never re-walk 400 pages of issue events out of the
// shared installation credential — would silently stop holding on exactly the
// stage this change is unblocking. So the resolved path is asserted against the
// production layout method byte for byte, for coordinates that exercise every
// character the name sanitizer touches.
func TestBacklogHealthCursorKeyAddressesTheProductionPath(t *testing.T) {
	// stateclient must not import the instance layout, so its notion of the
	// subdirectory is a constant. This is the pin that keeps it honest.
	if stateclient.BacklogHealthCursorDirName != instance.BacklogHealthDirName {
		t.Fatalf("stateclient dir %q != instance dir %q",
			stateclient.BacklogHealthCursorDirName, instance.BacklogHealthDirName)
	}

	layout := instance.NewLayout(t.TempDir())
	cases := []struct {
		name   string
		gaggle string
		repo   providers.RepositoryRef
		label  string
	}{
		{
			name:   "production coordinates",
			gaggle: "goobers",
			repo:   providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"},
			label:  providers.LabelReady,
		},
		{
			// The gaggle the local CLI passes when it has none.
			name:  "no gaggle",
			repo:  providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"},
			label: providers.LabelReady,
		},
		{
			// A three-part provider key: the "/" separators the layout
			// sanitizes are the ones a naive key builder would turn into
			// extra path elements.
			name:   "project-scoped repository",
			gaggle: "goobers",
			repo: providers.RepositoryRef{
				Provider: providers.ProviderADO, Owner: "org", Project: "proj", Name: "repo",
			},
			label: providers.LabelReady,
		},
		{
			// A coordinate carrying the "__" the file name joins on. The key
			// must still resolve to the same file, which is why the gaggle is
			// delimited separately rather than being the first "__" segment.
			name:   "underscored coordinates",
			gaggle: "goobers",
			repo:   providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "o", Name: "my__repo"},
			label:  "goobers:needs__triage",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			key := backlogHealthCursorKey(testCase.gaggle, testCase.repo, testCase.label)
			if !stateclient.ValidKey(key) {
				t.Fatalf("key %q is outside the closed namespace", key)
			}
			relative, err := stateclient.KeyRelativePath(key)
			if err != nil {
				t.Fatal(err)
			}
			got := filepath.Join(layout.SchedulerDir(), relative)
			want := layout.BacklogHealthCursorPath(testCase.gaggle,
				string(testCase.repo.Provider),
				backlogHealthCursorRepositoryKey(testCase.repo), testCase.label)
			if got != want {
				t.Fatalf("key %q resolves to\n %s\nwant\n %s", key, got, want)
			}
		})
	}
}

// TestBacklogHealthCursorKeyGetsItsOwnLock is the deliberate exception to
// finding 002's "same lock the key already used" rule, pinned so it stays
// deliberate.
//
// The ledger had NO cross-process lock before this change — a single in-process
// writer plus an atomic rename was enough. Over the plane it needs one, because
// the compare and the swap become two round trips into the daemon and without a
// daemon-held lock two callers can both pass the CAS. The lock it gets is its
// own rather than the default claims.lock: a persist follows a walk that may
// have crossed hundreds of provider pages, and dragging that into contention
// with every claim, release and blocked-record update would be a real
// regression for the claiming path, which shares none of this key's state.
func TestBacklogHealthCursorKeyGetsItsOwnLock(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := schedulerStateLock(layout)

	if err := lock(backlogHealthPlaneKey("goobers"), stateLockOperationBacklogHealthCursor, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(layout.SchedulerDir(), backlogHealthCursorLockFile)); err != nil {
		t.Fatalf("the ready-transition ledger did not take %s: %v", backlogHealthCursorLockFile, err)
	}
	if _, err := os.Stat(filepath.Join(layout.SchedulerDir(), claimLockFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the ready-transition ledger fell through to %s (err = %v); a provider walk's persist must not contend with claiming",
			claimLockFileName, err)
	}
}

// TestRunnerDrivenAndEngineDrivenLedgerAdvancesShareOneLock is the interleaving
// case for the ledger: the same assertion decision 005 R3 makes for the scan
// cursor, applied to the key this change admits.
//
// Two writers merge transitions into the SAME ledger concurrently — the
// runner-driven one in the daemon's own process through the file backend under
// backlog-health-cursor.lock, the engine-driven one in a stage pod through the
// plane, where the daemon takes that same lock on the caller's behalf. If the
// two ran in separate atomicity domains, merges would be lost: each would read
// the same ledger, fold in its own transitions, and one would overwrite the
// other. The assertion is exact — every transition must survive, and the high
// -water mark must be the maximum of both writers'.
func TestRunnerDrivenAndEngineDrivenLedgerAdvancesShareOneLock(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-engine")
	key := backlogHealthPlaneKey("goobers")

	engine := plane.client(t, "goobers", token)
	runner := plane.daemonStore(t)

	const perWriter = 12
	// Disjoint event-id ranges, so a lost merge is unambiguous rather than a
	// coincidence of two writers computing the same successor.
	advance := func(store stateclient.Store, base int) error {
		for i := 1; i <= perWriter; i++ {
			eventID := int64(base + i)
			err := store.Update(t.Context(), key, stateLockOperationBacklogHealthCursor,
				func(value stateclient.Value) ([]byte, bool, error) {
					cursor, reason := decodeBacklogHealthCursor(value, "goobers", backlogHealthPlaneRepo, providers.LabelReady)
					if reason != "" && value.Exists() {
						return nil, false, errors.New("ledger stopped decoding mid-flight: " + reason)
					}
					cursor.Transitions = mergeLabelTransitions(cursor.Transitions,
						[]providers.WorkItemLabelTransition{{
							EventID:    eventID,
							ItemID:     strconv.FormatInt(eventID, 10),
							Label:      providers.LabelReady,
							Added:      true,
							OccurredAt: time.Unix(eventID, 0).UTC(),
						}})
					if eventID > cursor.HighWaterEventID {
						cursor.HighWaterEventID = eventID
					}
					cursor.Schema = backlogHealthCursorSchema
					cursor.Gaggle, cursor.Provider = "goobers", string(backlogHealthPlaneRepo.Provider)
					cursor.Repository = backlogHealthCursorRepositoryKey(backlogHealthPlaneRepo)
					cursor.Label = providers.LabelReady
					encoded, err := encodeBacklogHealthCursor(cursor)
					return encoded, true, err
				})
			if err != nil {
				return err
			}
		}
		return nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	bases := []int{0, 100}
	for index, store := range []stateclient.Store{engine, runner} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[index] = advance(store, bases[index])
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	value, err := runner.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	cursor, reason := decodeBacklogHealthCursor(value, "goobers", backlogHealthPlaneRepo, providers.LabelReady)
	if reason != "" {
		t.Fatalf("final ledger does not decode: %s", reason)
	}
	if len(cursor.Transitions) != 2*perWriter {
		t.Fatalf("ledger holds %d transitions, want %d — a merge was lost across the two writers",
			len(cursor.Transitions), 2*perWriter)
	}
	if want := int64(bases[1] + perWriter); cursor.HighWaterEventID != want {
		t.Fatalf("high-water = %d, want %d", cursor.HighWaterEventID, want)
	}
}

// TestStatePlaneContainsTheLedgerToItsOwnGaggle is the containment this key
// needs and no other key in the namespace does.
//
// Every other scheduler-state key is gaggle-agnostic, so the route's own
// {gaggle} segment is the whole of its scope. The ready-transition ledger
// carries its gaggle IN the key, so route scope alone would let a pod contained
// to gaggle A name gaggle B's ledger and be served it — a cross-gaggle read of
// which items another gaggle has seen go ready, and a write that would corrupt
// that gaggle's resume point into re-walking its whole history.
func TestStatePlaneContainsTheLedgerToItsOwnGaggle(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-1")
	pod := plane.client(t, "goobers", token)

	own := backlogHealthPlaneKey("goobers")
	if _, err := pod.Put(t.Context(), own, []byte(`{"schema":"probe"}`), ""); err != nil {
		t.Fatalf("own-gaggle write: %v", err)
	}
	if _, err := pod.Get(t.Context(), own); err != nil {
		t.Fatalf("own-gaggle read: %v", err)
	}

	// The pod is correctly scoped; only the gaggle inside the key is foreign.
	// This is the case the route's own segment cannot catch.
	var planeErr *stateclient.Error
	for _, foreign := range []string{"other", "goobers-staging", "goober"} {
		key := backlogHealthPlaneKey(foreign)
		if _, err := pod.Get(t.Context(), key); !errors.As(err, &planeErr) || planeErr.Status != http.StatusForbidden {
			t.Fatalf("read of %q err = %v, want a typed 403", key, err)
		}
		if _, err := pod.Put(t.Context(), key, []byte(`{}`), ""); !errors.As(err, &planeErr) || planeErr.Status != http.StatusForbidden {
			t.Fatalf("write of %q err = %v, want a typed 403", key, err)
		}
		// Nothing may have been created for the foreign gaggle along the way.
		relative, err := stateclient.KeyRelativePath(key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(plane.layout.SchedulerDir(), relative)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused key created %s (err = %v)", relative, err)
		}
	}
}

// TestStatePlaneRefusesMalformedLedgerKeysServerSide keeps the subdirectory
// resolution from becoming an arbitrary-path primitive. The cursor prefix is
// the only key in the namespace that becomes more than one path element, so it
// is the only one where a traversal has anything to work with — and the refusal
// must not depend on a well-behaved client.
func TestStatePlaneRefusesMalformedLedgerKeysServerSide(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-1")

	// A file the plane must never serve, in the directory the prefix resolves
	// into and in the scheduler directory above it.
	cursorDir := filepath.Join(plane.layout.SchedulerDir(), stateclient.BacklogHealthCursorDirName)
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		filepath.Join(plane.layout.SchedulerDir(), "claims.json"),
		filepath.Join(cursorDir, "secret.json"),
	} {
		if err := os.WriteFile(secret, []byte(`{"claims":"secret"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, key := range []string{
		"backlog-health.goobers.%2e%2e%2fclaims.json",
		"backlog-health.goobers.." + string(filepath.Separator) + "claims.json",
		"backlog-health.goobers.secret.json",
		"backlog-health..github__o__r.json",
		"backlog-health.goobers.github__o.json",
		"backlog-health.goobers.github__o__r.json.tmp",
		"backlog-health.goobers",
		stateclient.BacklogHealthCursorKeyPrefix,
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
		if string(body) == `{"claims":"secret"}` {
			t.Fatalf("key %q served a file outside the closed namespace", key)
		}
	}
}

// TestBacklogHealthWritesItsLedgerThroughToTheDaemon is the end-to-end proof
// that closes #3948: with nothing but the environment a stage pod carries, a
// REAL `goobers backlog-health` cycle lands its ready-transition ledger in the
// DAEMON's scheduler directory and writes nothing beside the pod.
//
// This is the behaviour whose absence kept `backlog-health` on
// executor.StageRequiresInstanceRoot: an un-planed pod has no instance root, so
// the ledger resolved under "." and vanished with the container, which meant
// every cycle re-walked the repository's entire event history out of the shared
// installation credential — the exact burn #3392 exists to stop.
func TestBacklogHealthWritesItsLedgerThroughToTheDaemon(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-health")

	root, _ := backlogHealthFixture(t)
	stampStatePlaneEnv(t, plane, "goobers", token)

	report := runBacklogHealthCycle(t, root)
	if report.Scan.Deferred {
		t.Fatalf("the cycle deferred rather than persisting: %#v", report.Scan)
	}
	if report.Scan.LedgerSize == 0 {
		t.Fatalf("the cycle persisted an empty ledger: %#v", report.Scan)
	}

	// The daemon's file, at the production path — not a pod-local one.
	daemonPath := plane.layout.BacklogHealthCursorPath("goobers",
		string(providers.ProviderGitHub), "your-org/your-repo", providers.LabelReady)
	data, err := os.ReadFile(daemonPath)
	if err != nil {
		t.Fatalf("the ledger did not land on the daemon: %v", err)
	}
	var persisted backlogHealthCursor
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("the daemon's ledger does not parse: %v", err)
	}
	if persisted.Gaggle != "goobers" || persisted.HighWaterEventID <= 0 {
		t.Fatalf("the daemon's ledger is not this scan's: %#v", persisted)
	}

	// Nothing beside the pod. The pod's own tree is the instance root the
	// fixture builds, which is what a mis-resolved ledger would have used.
	podPath := instance.NewLayout(root).BacklogHealthCursorPath("goobers",
		string(providers.ProviderGitHub), "your-org/your-repo", providers.LabelReady)
	if _, err := os.Stat(podPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a ledger was written beside the pod at %s (err = %v); the plane write fell through", podPath, err)
	}

	// And the resume actually resumes: a second cycle through the same plane
	// reads the daemon's ledger rather than re-walking history.
	second := runBacklogHealthCycle(t, root)
	if second.Scan.Mode != backlogHealthScanIncremental {
		t.Fatalf("second cycle mode = %q (%s), want an incremental resume from the daemon's ledger",
			second.Scan.Mode, second.Scan.Reason)
	}
	if second.Scan.FromEventID != persisted.HighWaterEventID {
		t.Fatalf("second cycle resumed from %d, want the persisted high-water %d",
			second.Scan.FromEventID, persisted.HighWaterEventID)
	}
}

// TestBacklogHealthFailsClosedOnAPartialPlaneConfig is the refusal that keeps a
// half-configured pod from doing the un-planed thing quietly. An endpoint
// without a bearer must abort the cycle, not fall back to a scheduler-state
// file the pod does not have and nothing will ever read.
func TestBacklogHealthFailsClosedOnAPartialPlaneConfig(t *testing.T) {
	plane := newStatePlane(t)
	root, _ := backlogHealthFixture(t)
	t.Setenv(stateclient.EnvEndpoint, plane.server.URL)

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "backlog-health", root)
	if code == 0 {
		t.Fatalf("a partial plane config succeeded: stdout = %q, stderr = %q", stdout, stderr)
	}
	podPath := instance.NewLayout(root).BacklogHealthCursorPath("",
		string(providers.ProviderGitHub), "your-org/your-repo", providers.LabelReady)
	if _, err := os.Stat(podPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused cycle still wrote %s (err = %v)", podPath, err)
	}
}
