package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workflowcluster"
)

type labsClustersFunc func(context.Context) (workflowcluster.View, error)

func (f labsClustersFunc) WorkflowClusters(ctx context.Context) (workflowcluster.View, error) {
	return f(ctx)
}

func serveLabsClusters(t *testing.T, opts ...HandlerOption) *httptest.ResponseRecorder {
	t.Helper()
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.LabsWorkflowClustersPath, nil))
	return response
}

func TestLabsWorkflowClustersRouteServesView(t *testing.T) {
	model := workflowcluster.Model{Version: workflowcluster.ModelVersion, Population: 3, Clusters: []workflowcluster.Cluster{{ID: "c0", Members: []string{"a/x", "a/y", "b/z"}}}, Noise: []string{}}
	service := labsClustersFunc(func(context.Context) (workflowcluster.View, error) {
		return workflowcluster.View{Status: workflowcluster.ViewClustered, Population: 3, MinClusterSize: 3, Model: &model}, nil
	})
	response := serveLabsClusters(t, WithLabsWorkflowClusters(service))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var view workflowcluster.View
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Status != workflowcluster.ViewClustered || view.Model == nil || len(view.Model.Clusters) != 1 {
		t.Fatalf("view = %+v", view)
	}
}

func TestLabsWorkflowClustersRouteReportsUnavailableAndFailures(t *testing.T) {
	if response := serveLabsClusters(t); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired status = %d, body = %s", response.Code, response.Body.String())
	}
	failing := labsClustersFunc(func(context.Context) (workflowcluster.View, error) {
		return workflowcluster.View{}, errors.New("config has validation errors")
	})
	if response := serveLabsClusters(t, WithLabsWorkflowClusters(failing)); response.Code != http.StatusInternalServerError {
		t.Fatalf("failing status = %d, body = %s", response.Code, response.Body.String())
	}
}
