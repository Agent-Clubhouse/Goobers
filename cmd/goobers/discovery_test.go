package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

func TestDaemonReadHandlerOptionsPreserveDiscoveryWithoutReadModel(t *testing.T) {
	root := initDemo(t)
	expectedID, err := instance.ReadRootIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, readStore := range []*readmodel.Store{nil, store} {
		options := daemonReadHandlerOptions(root, &schedulerSetup{ReadModel: readStore})
		// Metadata must not invoke any method on the runtime reader.
		reader := struct{ readservice.Reader }{}
		handler, err := httpapi.NewHandler(reader, httpapi.AllowAll, log.New(io.Discard, "", 0), options...)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.CapabilitiesPath, nil))
		var document apicontract.CapabilityDocument
		if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || document.DaemonInstanceID != expectedID {
			t.Fatalf("metadata status=%d identity=%q", response.Code, document.DaemonInstanceID)
		}
		for _, route := range document.Routes {
			if route.ID == apicontract.RouteEvents && route.Available != (readStore != nil) {
				t.Fatalf("event availability does not match read model: %+v", route)
			}
			if route.ID == apicontract.RouteTelemetryStats && route.Available {
				t.Fatal("telemetry advertised without a configured store")
			}
		}
	}
}
