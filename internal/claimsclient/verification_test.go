package claimsclient

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestClaimVerificationRequestWireShape(t *testing.T) {
	assertVerificationWireShape(t, reflect.TypeFor[claimVerificationRequest](), reflect.TypeFor[httpapi.ClaimVerificationRequest]())
	now := time.Now().UTC()
	original := claimVerificationRequest{RunID: "caller", Gaggle: "g", Provider: "github", ItemID: "7", OwnerRunID: "owner", ClaimedAt: now.Add(-time.Minute), Observation: localscheduler.ClaimVerification{State: "ownership-mismatch", ObservedAt: now, ProviderRunID: "other"}}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var server httpapi.ClaimVerificationRequest
	if err := json.Unmarshal(data, &server); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(server)
	if err != nil {
		t.Fatal(err)
	}
	var result claimVerificationRequest
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, original) {
		t.Fatalf("wire lost populated fields: %+v != %+v", result, original)
	}
}

func assertVerificationWireShape(t *testing.T, a, b reflect.Type) {
	t.Helper()
	if a == b {
		return
	}
	if a.Kind() != reflect.Struct || b.Kind() != reflect.Struct {
		t.Fatalf("wire type drift: %v / %v", a, b)
	}
	fields := func(typ reflect.Type) map[string]reflect.Type {
		result := map[string]reflect.Type{}
		for i := range typ.NumField() {
			field := typ.Field(i)
			if tag := field.Tag.Get("json"); tag != "-" {
				result[tag] = field.Type
			}
		}
		return result
	}
	left, right := fields(a), fields(b)
	if len(left) != len(right) {
		t.Fatalf("wire field count drift: %v / %v", a, b)
	}
	for tag, typ := range left {
		mirror, ok := right[tag]
		if !ok {
			t.Fatalf("missing wire field %s", tag)
		}
		assertVerificationWireShape(t, typ, mirror)
	}
}

func TestClaimVerificationHTTPAttributionAndFilePersistence(t *testing.T) {
	ctx := context.Background()
	file, err := NewFile(FileConfig{LedgerPath: filepath.Join(t.TempDir(), "claims.json")})
	if err != nil {
		t.Fatal(err)
	}
	key := Key{Gaggle: "g", Provider: "github", ExternalID: "7"}
	if ok, _, err := file.ClaimScoped(ctx, key, "owner", "w", time.Hour); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	entries, err := file.ForRunAll(ctx, "owner")
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %+v %v", entries, err)
	}
	entry := entries[0]
	observation := localscheduler.ClaimVerification{State: "verified", ObservedAt: time.Now().UTC(), ProviderRunID: "owner"}
	plane, client := newFakePlane(t, func(path string, body map[string]any) (int, any) {
		if path != apicontract.ClaimVerifyPath || body["runId"] != "run-1" || body["ownerRunId"] != "owner" {
			t.Errorf("wrong request: %s %+v", path, body)
		}
		return http.StatusOK, map[string]bool{"ok": true}
	})
	if ok, err := client.RecordClaimVerification(ctx, entry, observation); err != nil || !ok {
		t.Fatalf("HTTP record: %v %v", ok, err)
	}
	if requests := plane.recorded(); len(requests) != 1 || requests[0].bearer != "claims-token" {
		t.Fatalf("missing bearer: %+v", requests)
	}
	if ok, err := file.RecordClaimVerification(ctx, entry, observation); err != nil || !ok {
		t.Fatalf("file record: %v %v", ok, err)
	}
	listing, err := file.ListNamespace(ctx, "g", "github")
	if err != nil || len(listing.Entries) != 1 || listing.Entries[0].Verification != observation || listing.History[0].Verification != observation || !listing.Entries[0].ExpiresAt.Equal(entry.ExpiresAt) {
		t.Fatalf("persistence changed lease or lost observation: %+v %v", listing, err)
	}
}
