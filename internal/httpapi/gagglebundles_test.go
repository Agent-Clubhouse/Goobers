package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/gagglebundle"
)

type fakeGaggleBundles struct {
	exported apiv1.GaggleBundle
	imported apiv1.GaggleBundleImportResult
	err      error
	name     string
	input    apiv1.GaggleBundleImportRequest
}

func (f *fakeGaggleBundles) ExportGaggle(_ context.Context, name string) (apiv1.GaggleBundle, error) {
	f.name = name
	return f.exported, f.err
}

func (f *fakeGaggleBundles) ImportGaggle(_ context.Context, input apiv1.GaggleBundleImportRequest) (apiv1.GaggleBundleImportResult, error) {
	f.input = input
	return f.imported, f.err
}

func TestGaggleBundleRoutesFailClosedWithoutService(t *testing.T) {
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, apicontract.V1Prefix+"/gaggles/example/bundle", nil),
		newGaggleBundleImportRequest(t, apiv1.GaggleBundleImportRequest{}),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, body = %s", request.Method, response.Code, response.Body)
		}
		if code := errorCode(t, response); code != "gaggle_bundles_unavailable" {
			t.Fatalf("code = %q", code)
		}
	}
}

func TestGaggleBundleExportUsesAuthorizedRoute(t *testing.T) {
	bundle := apiv1.GaggleBundle{
		APIVersion: apiv1.GaggleBundleAPIVersion, Kind: apiv1.GaggleBundleKind,
		SchemaVersion: apiv1.GaggleBundleSchemaVersion,
	}
	service := &fakeGaggleBundles{exported: bundle}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithGaggleBundles(service))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.V1Prefix+"/gaggles/example/bundle", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body)
	}
	if service.name != "example" {
		t.Fatalf("export name = %q", service.name)
	}
}

func TestGaggleBundleImportDecodesCompleteRequest(t *testing.T) {
	now := time.Now().UTC()
	input := apiv1.GaggleBundleImportRequest{
		Name: "copy",
		Bundle: apiv1.GaggleBundle{
			APIVersion: apiv1.GaggleBundleAPIVersion, Kind: apiv1.GaggleBundleKind,
			SchemaVersion: apiv1.GaggleBundleSchemaVersion, ExportedAt: now,
		},
	}
	service := &fakeGaggleBundles{imported: apiv1.GaggleBundleImportResult{Name: "copy", ImportedAt: now}}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithGaggleBundles(service))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newGaggleBundleImportRequest(t, input))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body)
	}
	if service.input.Name != "copy" || service.input.Bundle.Kind != apiv1.GaggleBundleKind {
		t.Fatalf("decoded input = %+v", service.input)
	}
}

func TestGaggleBundleImportBodyLimit(t *testing.T) {
	service := &fakeGaggleBundles{}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithGaggleBundles(service))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		size   int
		status int
		code   string
	}{
		{name: "exact limit", size: maxGaggleBundleBody, status: http.StatusOK},
		{name: "over limit after valid object", size: maxGaggleBundleBody + 1, status: http.StatusRequestEntityTooLarge, code: "gaggle_bundle_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := "{}" + strings.Repeat(" ", test.size-2)
			request := httptest.NewRequest(http.MethodPost, apicontract.GaggleBundleImportPath, strings.NewReader(body))
			request.Host = "127.0.0.1:8080"
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.status, response.Body)
			}
			if test.code != "" && errorCode(t, response) != test.code {
				t.Fatalf("code = %q, want %q", errorCode(t, response), test.code)
			}
		})
	}
}

func TestGaggleBundleErrorsAreExplicit(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{fmtError(gagglebundle.ErrGaggleNotFound), http.StatusNotFound, "gaggle_not_found"},
		{fmtError(gagglebundle.ErrNameConflict), http.StatusConflict, "gaggle_name_conflict"},
		{fmtError(gagglebundle.ErrRepositoryAuthorization), http.StatusPreconditionFailed, "repository_authorization_required"},
		{fmtError(gagglebundle.ErrInvalidBundle), http.StatusUnprocessableEntity, "invalid_gaggle_bundle"},
	}
	for _, test := range tests {
		service := &fakeGaggleBundles{err: test.err}
		handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithGaggleBundles(service))
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.V1Prefix+"/gaggles/example/bundle", nil))
		if response.Code != test.status || errorCode(t, response) != test.code {
			t.Fatalf("err %v => status=%d code=%q body=%s", test.err, response.Code, errorCode(t, response), response.Body)
		}
	}
}

func newGaggleBundleImportRequest(t *testing.T, input apiv1.GaggleBundleImportRequest) *http.Request {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, apicontract.GaggleBundleImportPath, bytes.NewReader(data))
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Content-Type", "application/json")
	return request
}

func fmtError(target error) error {
	return errors.Join(target, errors.New("detail"))
}
