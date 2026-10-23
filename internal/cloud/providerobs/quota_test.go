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

func newQuota(now time.Time) (Quota, *localscheduler.ProviderQuotaState) {
	state := localscheduler.NewProviderQuotaState()
	return Quota{State: state, Now: func() time.Time { return now }}, state
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

func TestRestoreSkipsExpiredForeignAndFutureVersions(t *testing.T) {
	q, state := newQuota(epoch0)
	expired, _ := json.Marshal(quotaPayload{Version: quotaVersion, Provider: "github", Remaining: 0, ResetAt: epoch0.Add(-time.Minute)})
	future, _ := json.Marshal(map[string]any{"version": 2, "provider": "ado", "remaining": 1, "resetAt": epoch0.Add(time.Hour), "extra": true})
	err := q.Restore(t.Context(), []control.Observation{
		{Key: "provider-quota/github", Payload: expired, ObservedAt: epoch0},
		{Key: "provider-quota/ado", Payload: future, ObservedAt: epoch0},
		{Key: "some-other-adapter/x", Payload: json.RawMessage(`{"anything":1}`), ObservedAt: epoch0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if windows := state.Windows(); len(windows) != 0 {
		t.Fatalf("windows = %+v, want none restored", windows)
	}
}

func TestRestoreRejectsCorruptKnownVersion(t *testing.T) {
	q, _ := newQuota(epoch0)
	for name, payload := range map[string]string{
		"not json":        `{`,
		"negative":        `{"version":1,"provider":"github","remaining":-1,"resetAt":"2030-01-01T00:00:00Z"}`,
		"key mismatch":    `{"version":1,"provider":"ado","remaining":1,"resetAt":"2030-01-01T00:00:00Z"}`,
		"missing resetAt": `{"version":1,"provider":"github","remaining":1}`,
	} {
		err := q.Restore(t.Context(), []control.Observation{{Key: "provider-quota/github", Payload: json.RawMessage(payload), ObservedAt: epoch0}})
		if err == nil {
			t.Fatalf("%s: corrupt payload accepted", name)
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
	if _, err := (Quota{}).Capture(context.Background()); err == nil {
		t.Fatal("capture without state succeeded")
	}
	if err := (Quota{}).Restore(context.Background(), nil); err == nil {
		t.Fatal("restore without state succeeded")
	}
}
