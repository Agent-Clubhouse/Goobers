package providerobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/localscheduler"
)

var epoch0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func newQuota(now time.Time) (*Quota, *localscheduler.ProviderQuotaState) {
	state := localscheduler.NewProviderQuotaState()
	return &Quota{State: state, Now: func() time.Time { return now }}, state
}

func TestCaptureRestoreRoundTripPreservesBudget(t *testing.T) {
	from, state := newQuota(epoch0)
	reset := epoch0.Add(time.Hour)
	state.Record(apiv1.ProviderGitHub, 40, reset)
	state.ReserveCurrentPolls(apiv1.ProviderGitHub, 15) // 25 left
	snapshot, err := from.Capture(t.Context())
	if err != nil || len(snapshot) != 1 || snapshot[0].Key != "provider-quota/github" {
		t.Fatalf("capture = %+v %v", snapshot, err)
	}
	to, restored := newQuota(epoch0.Add(time.Minute))
	if err := to.Restore(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	budget := restored.ReserveCurrentPolls(apiv1.ProviderGitHub, 100)
	if !budget.Known || budget.Allowed != 25 || !budget.ResetAt.Equal(reset) {
		t.Fatalf("restored budget = %+v, want 25 remaining in the same window", budget)
	}
}

func TestRestoreNeverAddsBudgetBack(t *testing.T) {
	q, state := newQuota(epoch0)
	reset := epoch0.Add(time.Hour)
	state.Record(apiv1.ProviderGitHub, 3, reset)
	stale, _ := json.Marshal(quotaPayload{Version: quotaVersion, Provider: "github", Remaining: 50, ResetAt: reset})
	if err := q.Restore(t.Context(), []control.Observation{{Key: "provider-quota/github", Payload: stale, ObservedAt: epoch0}}); err != nil {
		t.Fatal(err)
	}
	if budget := state.ReserveCurrentPolls(apiv1.ProviderGitHub, 100); budget.Allowed != 3 {
		t.Fatalf("a replayed snapshot raised the budget to %d", budget.Allowed)
	}
}

// A newer binary's payload (different shape, higher version) must not fail the
// restore: that would fail RebuildBeforeReap and a mixed-version failover could
// never build an epoch. Foreign and unknown-version rows are skipped by Restore
// and carried through by Capture so Replace does not delete them.
func TestRestoreIgnoresUnknownVersionsAndPassesThemThrough(t *testing.T) {
	q, state := newQuota(epoch0)
	reset := epoch0.Add(time.Hour)
	// Version 2 has an incompatible shape: "remaining" is an object.
	future := json.RawMessage(`{"version":2,"provider":"ado","remaining":{"core":1},"resetAt":"` + reset.Format(time.RFC3339) + `"}`)
	foreign := json.RawMessage(`{"anything":1}`)
	expired, _ := json.Marshal(quotaPayload{Version: quotaVersion, Provider: "github", Remaining: 0, ResetAt: epoch0.Add(-time.Minute)})
	err := q.Restore(t.Context(), []control.Observation{
		{Key: "provider-quota/github", Payload: expired, ObservedAt: epoch0},
		{Key: "provider-quota/ado", Payload: future, ObservedAt: epoch0},
		{Key: "some-other-adapter/x", Payload: foreign, ObservedAt: epoch0},
	})
	if err != nil {
		t.Fatalf("restore failed on a newer binary's row: %v", err)
	}
	if windows := state.Windows(); len(windows) != 0 {
		t.Fatalf("windows = %+v, want none restored", windows)
	}
	snapshot, err := q.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, observation := range snapshot {
		got[observation.Key] = string(observation.Payload)
	}
	if len(got) != 2 || got["provider-quota/ado"] != string(future) || got["some-other-adapter/x"] != string(foreign) {
		t.Fatalf("capture = %v, want the unknown-version and foreign rows passed through untouched", got)
	}
	// Once this binary has a current window for a key, it supersedes a stale
	// unknown-version row for the same key.
	state.Record("ado", 4, reset)
	snapshot, _ = q.Capture(t.Context())
	for _, observation := range snapshot {
		if observation.Key == "provider-quota/ado" && string(observation.Payload) == string(future) {
			t.Fatal("a current window did not supersede the unknown-version row")
		}
	}
}

// A corrupt row of the known version is reported and skipped; the ledger is an
// optimisation and must never keep a controller from serving.
func TestRestoreReportsAndSkipsCorruptKnownVersion(t *testing.T) {
	for name, payload := range map[string]string{
		"not json":        `{`,
		"negative":        `{"version":1,"provider":"github","remaining":-1,"resetAt":"2030-01-01T00:00:00Z"}`,
		"key mismatch":    `{"version":1,"provider":"ado","remaining":1,"resetAt":"2030-01-01T00:00:00Z"}`,
		"missing resetAt": `{"version":1,"provider":"github","remaining":1}`,
		"wrong type":      `{"version":1,"provider":"github","remaining":"many","resetAt":"2030-01-01T00:00:00Z"}`,
	} {
		q, state := newQuota(epoch0)
		var reported []error
		q.Report = func(err error) { reported = append(reported, err) }
		good, _ := json.Marshal(quotaPayload{Version: quotaVersion, Provider: "ado", Remaining: 7, ResetAt: epoch0.Add(time.Hour)})
		err := q.Restore(t.Context(), []control.Observation{
			{Key: "provider-quota/github", Payload: json.RawMessage(payload), ObservedAt: epoch0},
			{Key: "provider-quota/ado", Payload: good, ObservedAt: epoch0},
		})
		if err != nil {
			t.Fatalf("%s: corrupt row failed the restore: %v", name, err)
		}
		if len(reported) != 1 {
			t.Fatalf("%s: reported = %v, want exactly one finding", name, reported)
		}
		if windows := state.Windows(); len(windows) != 1 || windows[0].Provider != "ado" || windows[0].Remaining != 7 {
			t.Fatalf("%s: windows = %+v, want only the good row restored", name, windows)
		}
	}
}

func TestCaptureOmitsExpiredAndRetiredWindows(t *testing.T) {
	q, state := newQuota(epoch0)
	state.Record(apiv1.ProviderGitHub, 5, epoch0.Add(time.Hour))
	state.Record("ado", 5, epoch0.Add(-time.Minute))
	state.ResetIfDue("ado", epoch0)
	snapshot, err := q.Capture(t.Context())
	if err != nil || len(snapshot) != 1 || !strings.HasSuffix(snapshot[0].Key, "github") {
		t.Fatalf("capture = %+v %v, want only the active github window", snapshot, err)
	}
}

func TestNilStateFailsClosed(t *testing.T) {
	if _, err := (&Quota{}).Capture(context.Background()); err == nil {
		t.Fatal("capture without state succeeded")
	}
	if err := (&Quota{}).Restore(context.Background(), nil); err == nil {
		t.Fatal("restore without state succeeded")
	}
}

// Rows this adapter does not own are carried forward only while their writer is
// plausibly alive; a day-old orphan (a rolled-back newer binary) ages out.
func TestPassthroughRowsAgeOut(t *testing.T) {
	q, _ := newQuota(epoch0)
	fresh := control.Observation{Key: "other/fresh", Payload: json.RawMessage(`{"a":1}`), ObservedAt: epoch0.Add(-time.Hour)}
	stale := control.Observation{Key: "other/stale", Payload: json.RawMessage(`{"a":2}`), ObservedAt: epoch0.Add(-25 * time.Hour)}
	staleV2 := control.Observation{Key: "provider-quota/ado", Payload: json.RawMessage(`{"version":2}`), ObservedAt: epoch0.Add(-48 * time.Hour)}
	if err := q.Restore(t.Context(), []control.Observation{fresh, stale, staleV2}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := q.Capture(t.Context())
	if err != nil || len(snapshot) != 1 || snapshot[0].Key != "other/fresh" {
		t.Fatalf("capture = %+v %v, want only the fresh foreign row", snapshot, err)
	}
}
