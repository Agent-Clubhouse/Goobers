package providerscontract

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// withADOWorkItemsBatch answers POST .../_apis/wit/workitemsbatch, which the
// ADO provider hydrates WIQL hits through (ADO-N33), from the fixture's own
// GET .../_apis/wit/workitems/{id} handlers. An id whose GET answers 404
// comes back as null, as errorPolicy=Omit returns it; any other non-200
// answer fails the whole batch with that status.
func withADOWorkItemsBatch(t *testing.T, next http.Handler) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix, ok := strings.CutSuffix(r.URL.Path, "/_apis/wit/workitemsbatch")
		if !ok || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		var body struct {
			IDs         []int  `json:"ids"`
			Expand      string `json:"$expand"`
			ErrorPolicy string `json:"errorPolicy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.IDs) == 0 || len(body.IDs) > 200 ||
			body.Expand != "Relations" || body.ErrorPolicy != "Omit" {
			t.Errorf("workitemsbatch body = %+v (decode error %v), want 1..200 ids, $expand Relations, errorPolicy Omit", body, err)
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		values := make([]json.RawMessage, 0, len(body.IDs))
		for _, id := range body.IDs {
			target := prefix + "/_apis/wit/workitems/" + strconv.Itoa(id) + "?%24expand=Relations&api-version=7.1"
			itemReq := httptest.NewRequest(http.MethodGet, target, nil)
			itemReq.Header = r.Header.Clone()
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, itemReq)
			switch rec.Code {
			case http.StatusOK:
				values = append(values, json.RawMessage(rec.Body.Bytes()))
			case http.StatusNotFound:
				values = append(values, json.RawMessage("null"))
			default:
				http.Error(w, rec.Body.String(), rec.Code)
				return
			}
		}
		writeJSON(t, w, map[string]interface{}{"count": len(values), "value": values})
	})
}
