package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/fleet"
	"github.com/goobers/goobers/internal/instance"
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

type labsAssociationFunc func(string) (fleet.Association, error)

func (f labsAssociationFunc) LoadAssociation(root string) (fleet.Association, error) {
	return f(root)
}

// labsWorkflowFeatureSentinels are the configured names and goal text the
// privacy contract proves never leave the instance (#8375).
var labsWorkflowFeatureSentinels = map[string]string{
	"example":           "zqsentinelgaggle",
	"default-implement": "zqsentinelworkflow",
	"coder":             "zqsentinelgoober",
	"query-backlog":     "zqsentinelstage",
	"Implement the claimed backlog item and commit the change.": "ZQSENTINEL goal text never served.",
}

// writeSentinelWorkflowInstance scaffolds a valid instance whose gaggle,
// workflow, goober, stage names and a goal carry sentinel strings.
func writeSentinelWorkflowInstance(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	configDir := instance.NewLayout(root).ConfigDir()
	manifestPath := filepath.Join(configDir, "manifest.yaml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.Replace(string(manifest), "    - example\n", "    - "+labsWorkflowFeatureSentinels["example"]+"\n", 1)
	if renamed == string(manifest) {
		t.Fatalf("scaffolded manifest missing gaggle reference:\n%s", manifest)
	}
	if err := os.WriteFile(manifestPath, []byte(renamed), 0o644); err != nil {
		t.Fatal(err)
	}
	gaggles := filepath.Join(configDir, "gaggles")
	gaggleDir := filepath.Join(gaggles, labsWorkflowFeatureSentinels["example"])
	if err := os.Rename(filepath.Join(gaggles, "example"), gaggleDir); err != nil {
		t.Fatal(err)
	}
	gooberDir := filepath.Join(gaggleDir, "goobers", labsWorkflowFeatureSentinels["coder"])
	if err := os.Rename(filepath.Join(gaggleDir, "goobers", "coder"), gooberDir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(gaggleDir, "gaggle.yaml"),
		filepath.Join(gaggleDir, "workflows", "default-implement.yaml"),
		filepath.Join(gooberDir, "goober.yaml"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for original, sentinel := range labsWorkflowFeatureSentinels {
			text = strings.ReplaceAll(text, original, sentinel)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLabsWorkflowFeaturesRouteServesOnlySnapshotSchemaAndAudits(t *testing.T) {
	root := writeSentinelWorkflowInstance(t)
	points, err := workflowcluster.ConfiguredPoints(root)
	if err != nil || len(points) != 1 {
		t.Fatalf("fixture points = %d, err = %v", len(points), err)
	}
	if want := labsWorkflowFeatureSentinels["example"] + "/" + labsWorkflowFeatureSentinels["default-implement"]; points[0].ID != want {
		t.Fatalf("fixture point ID = %q, want %q", points[0].ID, want)
	}
	source, err := workflowcluster.NewFleetSnapshotSource(root, labsAssociationFunc(func(string) (fleet.Association, error) {
		return fleet.Association{
			InstanceID: "instance-1", FleetID: "fleet-1",
			Projections: []string{fleet.ProjectionLabsWorkflowFeatures},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	response := serveLabsWorkflowFeatures(t, source, apicontract.LabsWorkflowFeaturesPath+"?features=structural", &logs)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := strings.ToLower(response.Body.String())
	for _, sentinel := range labsWorkflowFeatureSentinels {
		if strings.Contains(body, strings.ToLower(sentinel)) {
			t.Fatalf("response leaked sentinel %q: %s", sentinel, response.Body.String())
		}
		if strings.Contains(logs.String(), sentinel) {
			t.Fatalf("audit log leaked sentinel %q: %q", sentinel, logs.String())
		}
	}
	for _, leak := range []string{"zqsentinel", "never served"} {
		if strings.Contains(body, leak) {
			t.Fatalf("response leaked %q: %s", leak, response.Body.String())
		}
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
	if !strings.Contains(logs.String(), `principal="local" workflows=1`) {
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
