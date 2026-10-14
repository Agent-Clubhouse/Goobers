package claimsclient

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/claimrelease"
)

func TestHTTPReleaseCarriesReasonOnTheWire(t *testing.T) {
	plane, client := newFakePlane(t, func(string, map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"ok": true}
	})
	reason := claimrelease.Failed.Because("agent_exit", "exit 2 in /home/op/work/tree").At("implement")
	if err := client.ReleaseScoped(context.Background(), Key{Gaggle: "g", Provider: "github", ExternalID: "7"}, "run-1", reason); err != nil {
		t.Fatal(err)
	}
	got := plane.recorded()
	wire, ok := got[0].body["reason"].(map[string]any)
	if !ok || wire["disposition"] != "failed" || wire["stage"] != "implement" || wire["code"] != "agent_exit" {
		t.Fatalf("release body reason = %v", got[0].body["reason"])
	}
	if summary, _ := wire["summary"].(string); strings.Contains(summary, "/home/op") {
		t.Fatalf("client sent an unsanitized summary: %q", summary)
	}
}

// An older daemon decodes strictly and refuses the reason field before any
// handler runs; the claim must still be surrendered, without the reason.
func TestHTTPReleaseFallsBackForADaemonWithoutReasons(t *testing.T) {
	plane, client := newFakePlane(t, func(_ string, body map[string]any) (int, any) {
		if _, has := body["reason"]; has {
			return http.StatusBadRequest, map[string]any{"error": map[string]any{
				"code": "invalid_request", "message": `invalid JSON request body: json: unknown field "reason"`}}
		}
		return http.StatusOK, map[string]any{"ok": true, "released": []Entry{{ItemID: "7", RunID: "run-1"}}}
	})
	ctx := context.Background()
	if err := client.ReleaseScoped(ctx, Key{Gaggle: "g", Provider: "github", ExternalID: "7"}, "run-1", claimrelease.Completed.Reason()); err != nil {
		t.Fatalf("ReleaseScoped against an older daemon: %v", err)
	}
	all, err := client.ReleaseAllForRun(ctx, "run-1", claimrelease.Completed.Reason())
	if err != nil || len(all) != 1 {
		t.Fatalf("ReleaseAllForRun against an older daemon = %+v, %v", all, err)
	}
	got := plane.recorded()
	if len(got) != 4 {
		t.Fatalf("recorded %d requests, want one refused and one plain attempt per release", len(got))
	}
	for i, request := range got {
		if _, has := request.body["reason"]; has != (i%2 == 0) {
			t.Fatalf("request %d reason presence = %v: %v", i, has, request.body)
		}
	}
}

func TestHTTPReleaseDoesNotRetryOtherRefusals(t *testing.T) {
	plane, client := newFakePlane(t, func(string, map[string]any) (int, any) {
		return http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "invalid_request", "message": "runId is required"}}
	})
	err := client.ReleaseScoped(context.Background(), Key{Gaggle: "g", Provider: "github", ExternalID: "7"}, "run-1", claimrelease.Completed.Reason())
	if err == nil || len(plane.recorded()) != 1 {
		t.Fatalf("err = %v after %d requests, want one refused request", err, len(plane.recorded()))
	}
}
