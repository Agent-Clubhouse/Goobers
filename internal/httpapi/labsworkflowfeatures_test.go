package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workflowcluster"
)

type labsWorkflowFeaturesStub struct {
	snapshot workflowcluster.FleetSnapshot
	err      error
	features []string
}

func (s *labsWorkflowFeaturesStub) WorkflowFeatures(_ context.Context, features string) (workflowcluster.FleetSnapshot, error) {
	s.features = append(s.features, features)
	return s.snapshot, s.err
}

func (s *labsWorkflowFeaturesStub) WorkflowFeaturesAvailability(context.Context) error {
	return s.err
}

func serveLabsWorkflowFeatures(t *testing.T, service LabsWorkflowFeaturesService, target string, logs *bytes.Buffer, opts ...HandlerOption) *httptest.ResponseRecorder {
	t.Helper()
	if logs == nil {
		logs = &bytes.Buffer{}
	}
	if service != nil {
		opts = append(opts, WithLabsWorkflowFeatures(service))
	}
	handler, err := NewHandler(&fakeReader{}, AllowAll, log.New(logs, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	return response
}

func TestLabsWorkflowFeaturesRouteGatesProjection(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"not associated", workflowcluster.ErrFleetNotAssociated},
		{"revoked", workflowcluster.ErrFleetAssociationRevoked},
		{"projection omitted", workflowcluster.ErrFleetProjectionNotGranted},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := serveLabsWorkflowFeatures(t, &labsWorkflowFeaturesStub{err: test.err},
				apicontract.LabsWorkflowFeaturesPath+"?features=structural", nil)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"projection_not_granted"`) {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
	if response := serveLabsWorkflowFeatures(t, nil, apicontract.LabsWorkflowFeaturesPath+"?features=structural", nil); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestLabsWorkflowFeaturesRouteValidatesQuery(t *testing.T) {
	for _, query := range []string{"", "?features=other", "?features=structural&features=behavior", "?features=structural&extra=x"} {
		service := &labsWorkflowFeaturesStub{}
		response := serveLabsWorkflowFeatures(t, service, apicontract.LabsWorkflowFeaturesPath+query, nil)
		if response.Code != http.StatusBadRequest || len(service.features) != 0 {
			t.Fatalf("query %q: status = %d, calls = %v, body = %s", query, response.Code, service.features, response.Body.String())
		}
	}
}

func TestLabsWorkflowFeaturesRouteServesOnlySnapshotSchemaAndAudits(t *testing.T) {
	const sentinel = "SENTINEL-workflow-gaggle-goober-stage-goal"
	snapshot, err := workflowcluster.NewFleetSnapshot(
		workflowcluster.StructuralFeatureVersion, "instance-1", "test", "fleet-1",
		[]workflowcluster.Point{{
			ID:          sentinel,
			Numeric:     map[string]float64{"nodes": 1},
			Categorical: map[string][]string{"goobers": {sentinel}},
		}},
		time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	service := &labsWorkflowFeaturesStub{snapshot: snapshot}
	var logs bytes.Buffer
	response := serveLabsWorkflowFeatures(t, service, apicontract.LabsWorkflowFeaturesPath+"?features=structural", &logs)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), sentinel) {
		t.Fatalf("response leaked sentinel: %s", response.Body.String())
	}
	var document map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, document, "version", "featureVersion", "instanceId", "goobersVersion", "generatedAt", "workflows")
	workflows, ok := document["workflows"].([]any)
	if !ok || len(workflows) != 1 {
		t.Fatalf("workflows = %#v", document["workflows"])
	}
	assertKeys(t, workflows[0].(map[string]any), "key", "shape", "numeric", "categorical")
	if !strings.Contains(logs.String(), `principal="local" workflows=1`) || strings.Contains(logs.String(), sentinel) {
		t.Fatalf("audit log = %q", logs.String())
	}
}

func TestLabsWorkflowFeaturesCapabilityReflectsAssociation(t *testing.T) {
	for _, test := range []struct {
		err       error
		available bool
		code      string
	}{
		{nil, true, ""},
		{workflowcluster.ErrFleetNotAssociated, false, "fleet_not_associated"},
		{workflowcluster.ErrFleetAssociationRevoked, false, "fleet_association_revoked"},
		{workflowcluster.ErrFleetProjectionNotGranted, false, "projection_not_granted"},
		{errors.New("read failed"), false, "association_unavailable"},
	} {
		service := &labsWorkflowFeaturesStub{err: test.err}
		response := serveLabsWorkflowFeatures(t, service, apicontract.CapabilitiesPath, nil,
			WithDiscoveryIdentity(DiscoveryIdentity{DaemonInstanceID: "instance-1"}))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var document apicontract.CapabilityDocument
		if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		capability := capabilityByID(t, document, apicontract.RouteLabsWorkflowFeatures)
		if capability.Available != test.available || capability.Code != test.code ||
			capability.Capability != "labs.workflowFeatures" || !capability.Remote {
			t.Fatalf("capability = %+v", capability)
		}
	}
}

func TestLabsWorkflowFeaturesRouteReportsSnapshotFailure(t *testing.T) {
	response := serveLabsWorkflowFeatures(t, &labsWorkflowFeaturesStub{err: errors.New("read failed")},
		apicontract.LabsWorkflowFeaturesPath+"?features=behavior", nil)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func assertKeys(t *testing.T, object map[string]any, keys ...string) {
	t.Helper()
	if len(object) != len(keys) {
		t.Fatalf("keys = %#v, want %v", object, keys)
	}
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			t.Fatalf("missing key %q in %#v", key, object)
		}
	}
}
