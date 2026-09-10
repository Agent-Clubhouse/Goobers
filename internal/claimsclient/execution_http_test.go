package claimsclient

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

func TestExecutionSnapshotWireAndMissingPolicy(t *testing.T) {
	assertVerificationWireShape(t, reflect.TypeFor[claimListRequest](), reflect.TypeFor[httpapi.ClaimListRequest]())
	for _, mode := range []string{"", "unknown", "local", "shared"} {
		t.Run("mode="+mode, func(t *testing.T) {
			_, client := newFakePlane(t, func(_ string, body map[string]any) (int, any) {
				if body["execution"] != true || body["includeHistory"] != true || body["scope"] != "run" {
					t.Errorf("incomplete own-run execution query: %+v", body)
				}
				return http.StatusOK, map[string]any{"claimVisibility": mode, "observedAt": time.Now(), "entries": []Entry{}}
			})
			got, _, err := client.ExecutionSnapshot(t.Context())
			valid := mode == "local" || mode == "shared"
			if (err == nil) != valid || (valid && got != mode) {
				t.Fatalf("policy=%q err=%v", got, err)
			}
		})
	}
}

func TestExecutionClockTranslationPreservesLeaseAndReleaseOrdering(t *testing.T) {
	started := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for _, skew := range []time.Duration{-12 * time.Hour, 12 * time.Hour} {
		observed := started.Add(skew)
		released := observed.Add(-time.Second)
		entries := []Entry{{ClaimedAt: observed.Add(-time.Minute), SharedDeadline: observed.Add(time.Minute), ExpiresAt: observed.Add(time.Minute), ReleasedAt: &released}, {}}
		translated := localExecutionTimes(entries, started, observed)
		entry := translated[0]
		if !entry.SharedDeadline.Equal(started.Add(time.Minute)) || !entry.ExpiresAt.Equal(entry.SharedDeadline) || !entry.ClaimedAt.Equal(started.Add(-time.Minute)) || !entry.ReleasedAt.Equal(started.Add(-time.Second)) {
			t.Fatalf("clock skew extended authority or lost release evidence: %+v", entry)
		}
		if !translated[1].SharedDeadline.IsZero() || translated[1].ReleasedAt != nil {
			t.Fatal("local claim was changed into shared authority")
		}
	}
}

func TestExecutionSnapshotSpendsTransitTimeAndRequiresServerClock(t *testing.T) {
	_, client := newFakePlane(t, func(_ string, _ map[string]any) (int, any) {
		observed := time.Now().Add(-12 * time.Hour)
		time.Sleep(50 * time.Millisecond)
		return http.StatusOK, claimListResponse{ClaimVisibility: "shared", ObservedAt: observed, Entries: []Entry{{SharedDeadline: observed.Add(10 * time.Millisecond), ExpiresAt: observed.Add(10 * time.Millisecond)}}}
	})
	_, listing, err := client.ExecutionSnapshot(t.Context())
	if err != nil || len(listing.Entries) != 1 || listing.Entries[0].SharedDeadline.After(time.Now()) {
		t.Fatalf("transit time granted new authority: %+v %v", listing, err)
	}
	_, noClock := newFakePlane(t, func(_ string, _ map[string]any) (int, any) {
		return http.StatusOK, claimListResponse{ClaimVisibility: "shared"}
	})
	if _, _, err := noClock.ExecutionSnapshot(t.Context()); err == nil {
		t.Fatal("missing server clock enabled shared execution")
	}
}
