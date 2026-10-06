package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
)

type gaggleHealthServiceFunc func(context.Context, string) (apiv1.GaggleHealthResponse, error)

func (f gaggleHealthServiceFunc) Health(ctx context.Context, gaggle string) (apiv1.GaggleHealthResponse, error) {
	return f(ctx, gaggle)
}

func TestGaggleHealthRouteServesControllerFreshness(t *testing.T) {
	now := time.Date(2026, 10, 6, 20, 0, 0, 0, time.UTC)
	var requested string
	service := gaggleHealthServiceFunc(func(_ context.Context, gaggle string) (apiv1.GaggleHealthResponse, error) {
		requested = gaggle
		return apiv1.GaggleHealthResponse{
			SchemaVersion: apiv1.GaggleHealthSchemaVersion,
			Health: apiv1.GaggleHealthSnapshot{
				SchemaVersion: apiv1.GaggleHealthSchemaVersion,
				Gaggle:        gaggle,
				State:         apiv1.GaggleHealthHealthy,
				UpdatedAt:     now,
				Active:        []apiv1.GaggleHealthFinding{},
				History:       []apiv1.GaggleHealthFinding{},
			},
			Controller: &apiv1.GaggleHealthControllerStatus{
				FreshAt: now, NextEvaluation: now.Add(time.Minute),
			},
		}, nil
	})
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithGaggleHealth(service))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, apicontract.V1Prefix+"/gaggles/alpha/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if requested != "alpha" {
		t.Fatalf("requested gaggle = %q, want alpha", requested)
	}
}
