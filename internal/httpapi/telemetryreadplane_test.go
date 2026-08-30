package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

// telemetryreadplane_test.go pins decision 005 R4 / finding 002 C3: a stage
// pod may GET the derived telemetry reads for ITS OWN gaggle and nothing
// else, and no human-facing behaviour changes.

func podPrincipalFor(runID string) *fakeAuthenticator {
	return &fakeAuthenticator{principal: &Principal{Subject: "run:" + runID, Issuer: PodPrincipalIssuer}}
}

func gaggleResolver(mapping map[string]string, err error) func(context.Context, string) (string, error) {
	return func(_ context.Context, runID string) (string, error) {
		if err != nil {
			return "", err
		}
		gaggle, ok := mapping[runID]
		if !ok {
			return "", errors.New("unknown run")
		}
		return gaggle, nil
	}
}

// TestPodPrincipalReachesOnlyTheRuledTelemetryReads pins the authorizer half:
// the ruling named stats and errors (plus the implementation-outcome evidence
// derived from the same rows), GET only. Everything else the pod could try on
// this surface — the error-signature aggregate, a non-GET method, an
// unrelated read route — stays refused before any handler runs.
func TestPodPrincipalReachesOnlyTheRuledTelemetryReads(t *testing.T) {
	authorizer := RequireRoles()
	pod := Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer}
	tests := []struct {
		name    string
		method  string
		path    string
		allowed bool
	}{
		{name: "stats GET", method: http.MethodGet, path: TelemetryStatsPath, allowed: true},
		{name: "errors GET", method: http.MethodGet, path: TelemetryErrorsPath, allowed: true},
		{name: "implementation outcomes GET", method: http.MethodGet, path: apicontract.TelemetryImplementationOutcomesPath, allowed: true},
		{name: "error signatures GET stays human-only", method: http.MethodGet, path: TelemetryErrorSignaturesPath, allowed: false},
		{name: "stats HEAD refused", method: http.MethodHead, path: TelemetryStatsPath, allowed: false},
		{name: "stats POST refused", method: http.MethodPost, path: TelemetryStatsPath, allowed: false},
		{name: "runs list still refused", method: http.MethodGet, path: RunsPath, allowed: false},
		{name: "instance still refused", method: http.MethodGet, path: InstancePath, allowed: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, pod))
			err := authorizer.Authorize(request)
			if test.allowed && err != nil {
				t.Fatalf("Authorize() = %v, want nil", err)
			}
			if !test.allowed && err == nil {
				t.Fatal("Authorize() = nil, want refusal")
			}
		})
	}
}

// TestPodTelemetryReadIsContainedToItsOwnGaggle walks the handler half. Every
// branch that cannot PROVE the read stays inside the pod's own gaggle must
// answer 403, and the refusal must never name the gaggle the pod failed to
// guess.
func TestPodTelemetryReadIsContainedToItsOwnGaggle(t *testing.T) {
	paths := []string{
		TelemetryStatsPath,
		TelemetryErrorsPath,
		apicontract.TelemetryImplementationOutcomesPath,
	}
	tests := []struct {
		name     string
		resolver func(context.Context, string) (string, error)
		query    string
		status   int
		code     string
	}{
		{
			name:     "own gaggle admitted",
			resolver: gaggleResolver(map[string]string{"run-1": "core"}, nil),
			query:    "?gaggle=core",
			status:   http.StatusOK,
		},
		{
			name:     "another gaggle refused",
			resolver: gaggleResolver(map[string]string{"run-1": "core"}, nil),
			query:    "?gaggle=platform",
			status:   http.StatusForbidden,
			code:     "gaggle_mismatch",
		},
		{
			name:     "unscoped read refused",
			resolver: gaggleResolver(map[string]string{"run-1": "core"}, nil),
			query:    "",
			status:   http.StatusForbidden,
			code:     "gaggle_required",
		},
		{
			name:     "empty gaggle parameter refused",
			resolver: gaggleResolver(map[string]string{"run-1": "core"}, nil),
			query:    "?gaggle=",
			status:   http.StatusForbidden,
			code:     "gaggle_required",
		},
		{
			name:     "unresolvable run refused",
			resolver: gaggleResolver(map[string]string{"run-9": "core"}, nil),
			query:    "?gaggle=core",
			status:   http.StatusForbidden,
			code:     "gaggle_mismatch",
		},
		{
			name:     "resolver failure refused",
			resolver: gaggleResolver(nil, errors.New("journal unreadable")),
			query:    "?gaggle=core",
			status:   http.StatusForbidden,
			code:     "gaggle_mismatch",
		},
		{
			name:     "ungaggled run refused",
			resolver: gaggleResolver(map[string]string{"run-1": ""}, nil),
			query:    "?gaggle=core",
			status:   http.StatusForbidden,
			code:     "gaggle_mismatch",
		},
		{
			name:     "no resolver wired refuses the whole plane",
			resolver: nil,
			query:    "?gaggle=core",
			status:   http.StatusForbidden,
			code:     "telemetry_scope_unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, path := range paths {
				options := []HandlerOption{WithAuthenticator(podPrincipalFor("run-1"))}
				if test.resolver != nil {
					options = append(options, WithPodRunGaggle(test.resolver))
				}
				handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), options...)
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path+test.query, nil))
				if response.Code != test.status {
					t.Fatalf("%s status = %d, want %d, body = %s", path, response.Code, test.status, response.Body)
				}
				if test.code == "" {
					continue
				}
				var envelope ErrorEnvelope
				if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Error.Code != test.code {
					t.Fatalf("%s error = %+v, want code %q", path, envelope.Error, test.code)
				}
				if message := envelope.Error.Message; strings.Contains(message, "core") || strings.Contains(message, "platform") {
					t.Fatalf("%s refusal message leaks a gaggle name: %q", path, message)
				}
			}
		})
	}
}

// TestHumanTelemetryReadsAreUnchanged is the parity half of the ruling: a
// human principal keeps the unscoped access it has always had on all four
// telemetry routes, with or without a pod-gaggle resolver wired, and the
// query reaches the reader exactly as before.
func TestHumanTelemetryReadsAreUnchanged(t *testing.T) {
	for _, name := range []string{"resolver wired", "no resolver"} {
		t.Run(name, func(t *testing.T) {
			reader := &fakeReader{}
			options := []HandlerOption{
				WithAuthenticator(&fakeAuthenticator{principal: &Principal{Subject: "viewer", Roles: []Role{RoleView}}}),
			}
			if name == "resolver wired" {
				options = append(options, WithPodRunGaggle(gaggleResolver(map[string]string{"run-1": "core"}, nil)))
			}
			handler, err := NewHandler(reader, RequireRoles(), discardLogger(), options...)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{
				TelemetryStatsPath,
				TelemetryErrorsPath,
				TelemetryErrorSignaturesPath,
				apicontract.TelemetryImplementationOutcomesPath,
			} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if response.Code != http.StatusOK {
					t.Fatalf("%s status = %d, body = %s", path, response.Code, response.Body)
				}
			}
			if reader.statsReq.Gaggle != "" || reader.errorsReq.Gaggle != "" || reader.outcomesReq.Gaggle != "" {
				t.Fatalf("human read was silently gaggle-scoped: %+v %+v %+v",
					reader.statsReq, reader.errorsReq, reader.outcomesReq)
			}
		})
	}
}

// TestImplementationOutcomesQueryParsing pins the new route's query surface:
// gaggle and since only, an unknown or duplicated parameter is a 400, and a
// bad timestamp never reaches the reader.
func TestImplementationOutcomesQueryParsing(t *testing.T) {
	reader := &fakeReader{outcomes: readservice.TelemetryImplementationOutcomesResult{
		Items: []readservice.TelemetryImplementationOutcome{{RunID: "run-7", ItemID: "42", Status: "failed"}},
	}}
	handler, err := NewHandler(reader, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	since := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		apicontract.TelemetryImplementationOutcomesPath+"?gaggle=core&since="+since.Format(time.RFC3339), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if reader.outcomesReq.Gaggle != "core" || !reader.outcomesReq.Since.Equal(since) {
		t.Fatalf("request = %+v", reader.outcomesReq)
	}
	var decoded readservice.TelemetryImplementationOutcomesResult
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 1 || decoded.Items[0].RunID != "run-7" {
		t.Fatalf("body = %+v", decoded)
	}

	for _, query := range []string{"?since=yesterday", "?workflow=implement", "?gaggle=a&gaggle=b"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
			apicontract.TelemetryImplementationOutcomesPath+query, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", query, response.Code)
		}
	}
}
