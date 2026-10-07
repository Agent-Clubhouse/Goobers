package stateclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func flakyState(t *testing.T, n int32, mode string) (*HTTP, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= n {
			if mode == "reset" {
				panic(http.ErrAbortHandler)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"class_saturated","message":"too many concurrent mutation requests; retry shortly"}}`)
			return
		}
		if r.Method == http.MethodGet {
			w.Header().Set(HeaderETag, `"abc"`)
			_, _ = io.WriteString(w, `{"v":1}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "t", Gaggle: "g", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return client, &calls
}

func TestGetRetriesSaturationAndStreamReset(t *testing.T) {
	for _, mode := range []string{"saturated", "reset"} {
		client, calls := flakyState(t, 2, mode)
		value, err := client.Get(context.Background(), KeyBlockedRecords)
		if err != nil || string(value.Data) != `{"v":1}` || calls.Load() != 3 {
			t.Fatalf("%s: value=%q err=%v calls=%d", mode, value.Data, err, calls.Load())
		}
	}
}

func TestPutRetriesRefusalButNotAmbiguousReset(t *testing.T) {
	client, calls := flakyState(t, 2, "saturated")
	if _, err := client.Put(context.Background(), KeyBlockedRecords, []byte(`{"v":2}`), ""); err != nil || calls.Load() != 3 {
		t.Fatalf("saturated put: err=%v calls=%d", err, calls.Load())
	}
	client, calls = flakyState(t, 1, "reset")
	_, err := client.Put(context.Background(), KeyBlockedRecords, []byte(`{"v":2}`), "")
	if err == nil || !strings.Contains(err.Error(), "INTERNAL_ERROR") || calls.Load() != 1 {
		t.Fatalf("reset put must not replay: err=%v calls=%d", err, calls.Load())
	}
}
