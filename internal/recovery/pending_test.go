package recovery

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

func TestHTTPArchiveSourceRequiresRecognizedPendingProtocol(t *testing.T) {
	for _, mode := range []string{"valid", "unknown-state", "wrong-code", "oversized", "wrong-status"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				state := PromotionCapacity
				code := OverflowPendingCode
				status := http.StatusConflict
				if mode == "unknown-state" {
					state = "private server detail"
				}
				if mode == "wrong-code" {
					code = "other-error"
				}
				if mode == "wrong-status" {
					status = http.StatusForbidden
				}
				w.Header().Set(PromotionStateHeader, state)
				w.WriteHeader(status)
				message := "private server detail"
				if mode == "oversized" {
					message = strings.Repeat("x", 8192)
				}
				_ = json.NewEncoder(w).Encode(apicontract.ErrorEnvelope{Error: apicontract.APIError{Code: code, Message: message}})
			}))
			defer server.Close()
			source := HTTPArchiveSource{BaseURL: server.URL, Token: "token", RunID: "receiving-run"}
			err := source.WithArchive(t.Context(), "repository", "7", func(Record, SourceRun, string) error { t.Fatal("pending response reached consumer"); return nil })
			var pending *OverflowPendingError
			if mode == "valid" {
				if !errors.As(err, &pending) || pending.PromotionState != PromotionCapacity {
					t.Fatal(err)
				}
			} else if err == nil || errors.As(err, &pending) {
				t.Fatalf("invalid pending protocol accepted: %v", err)
			}
			if strings.Contains(err.Error(), "private server detail") {
				t.Fatal("remote private error text leaked")
			}
		})
	}
}
