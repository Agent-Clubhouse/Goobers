package claimsclient

import (
	"net/http"
	"reflect"
	"testing"

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
				return http.StatusOK, map[string]any{"claimVisibility": mode, "entries": []Entry{}}
			})
			got, _, err := client.ExecutionSnapshot(t.Context())
			valid := mode == "local" || mode == "shared"
			if (err == nil) != valid || (valid && got != mode) {
				t.Fatalf("policy=%q err=%v", got, err)
			}
		})
	}
}
