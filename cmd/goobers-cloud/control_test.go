package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudauth "github.com/goobers/goobers/internal/cloud/podauth"
)

type controlTestLeadership struct{}

func (controlTestLeadership) Run(context.Context, func(context.Context, *lease.Handle) error) error {
	return lease.ErrHeld
}
func (controlTestLeadership) SelfFenced(*lease.Handle) bool { return true }

func TestControlBootRefusesSelfExecution(t *testing.T) {
	code, _, stderr := runForTest("control", "--self-execution", "allow")
	if code != 2 || !strings.Contains(stderr, "selfExecution: deny") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestControlReadinessRefusesMissingPlanesAndLease(t *testing.T) {
	host, err := control.New(control.Config{SelfExecution: "deny", Leadership: controlTestLeadership{}, SchemaCompatible: func(context.Context) error { return nil }, BuildEpoch: func(context.Context, *lease.Handle) (control.EpochServices, error) {
		return control.EpochServices{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	db := databaseServices{StartStatusPage: func(context.Context, string, int) ([]acceptance.StartStatus, error) { return nil, nil }}
	handler, err := controlHandler(db, controlPlanes{}, &cloudauth.Auth{}, host, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness=%d %s", response.Code, response.Body.String())
	}
}
