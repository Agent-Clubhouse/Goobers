package telemetryclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

func envFrom(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

// TestSelectFailsClosed pins the rule the whole package exists to hold: a
// stage that names a plane and cannot use it FAILS, it does not quietly read
// a rollup file the pod does not have and report "no evidence".
func TestSelectFailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		selected bool
		wantErr  error
	}{
		{
			name:     "no endpoint selects the local rollup",
			env:      map[string]string{EnvGaggle: "core"},
			selected: false,
		},
		{
			name:    "endpoint without a bearer refuses",
			env:     map[string]string{EnvEndpoint: "https://daemon.internal", EnvGaggle: "core"},
			wantErr: ErrEndpointWithoutToken,
		},
		{
			name:    "endpoint without a gaggle refuses",
			env:     map[string]string{EnvEndpoint: "https://daemon.internal", EnvToken: "t"},
			wantErr: ErrEndpointWithoutGaggle,
		},
		{
			name:    "whitespace is not a bearer",
			env:     map[string]string{EnvEndpoint: "https://daemon.internal", EnvToken: "   ", EnvGaggle: "core"},
			wantErr: ErrEndpointWithoutToken,
		},
		{
			name:     "telemetry-scoped pair selects the plane",
			env:      map[string]string{EnvEndpoint: "https://daemon.internal", EnvToken: "t", EnvGaggle: "core"},
			selected: true,
		},
		{
			name:     "control-plane spelling is the fallback",
			env:      map[string]string{EnvFallbackEndpoint: "https://daemon.internal", EnvFallbackToken: "t", EnvGaggle: "core"},
			selected: true,
		},
		{
			name:    "control-plane endpoint without its bearer refuses",
			env:     map[string]string{EnvFallbackEndpoint: "https://daemon.internal", EnvGaggle: "core"},
			wantErr: ErrEndpointWithoutToken,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, selected, err := Select(envFrom(test.env))
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("Select() error = %v, want %v", err, test.wantErr)
				}
				if client != nil || selected {
					t.Fatal("a refused selection must not return a usable client")
				}
				return
			}
			if err != nil {
				t.Fatalf("Select() = %v", err)
			}
			if selected != test.selected {
				t.Fatalf("selected = %v, want %v", selected, test.selected)
			}
			if selected && client.Gaggle() != "core" {
				t.Fatalf("gaggle = %q", client.Gaggle())
			}
		})
	}
}

// TestSelectPrefersTheTelemetryScopedPair proves the fallback never shadows an
// explicit telemetry bearer: a pod holding both must present the read-scoped
// one, not the pod token that could author its own outcome.
func TestSelectPrefersTheTelemetryScopedPair(t *testing.T) {
	var presented string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented = r.Header.Get("Authorization")
		writeOutcomes(w, nil)
	}))
	defer server.Close()

	client, selected, err := Select(envFrom(map[string]string{
		EnvEndpoint:         server.URL,
		EnvToken:            "read-scoped",
		EnvFallbackEndpoint: server.URL,
		EnvFallbackToken:    "pod-token",
		EnvGaggle:           "core",
	}))
	if err != nil || !selected {
		t.Fatalf("Select() = %v, %v", selected, err)
	}
	if _, err := client.ImplementationOutcomes(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if presented != "Bearer read-scoped" {
		t.Fatalf("Authorization = %q, want the read-scoped bearer", presented)
	}
}

func writeOutcomes(w http.ResponseWriter, items []ImplementationOutcome) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(implementationOutcomesResponse{Items: items})
}

// TestImplementationOutcomesScopesEveryRead proves the client never issues an
// unscoped read: the gaggle it was constructed with rides every request, and
// a bounded window is passed through as RFC3339.
func TestImplementationOutcomesScopesEveryRead(t *testing.T) {
	since := time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC)
	want := []ImplementationOutcome{{
		RunID: "run-3", ItemID: "912", Status: "failed",
		StartedAt: since, FinishedAt: since.Add(time.Hour),
		Stage: "implement", ErrorCode: "harness.crash", ErrorMessage: "boom",
		Gate: "review", Verdict: "reject",
	}}
	var gotPath, gotGaggle, gotSince string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotGaggle = r.URL.Query().Get("gaggle")
		gotSince = r.URL.Query().Get("since")
		writeOutcomes(w, want)
	}))
	defer server.Close()

	client, err := NewHTTP(Config{BaseURL: server.URL + "/", Token: "t", Gaggle: "core"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ImplementationOutcomes(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != apicontract.TelemetryImplementationOutcomesPath {
		t.Fatalf("path = %q", gotPath)
	}
	if gotGaggle != "core" {
		t.Fatalf("gaggle = %q, want the client's own", gotGaggle)
	}
	parsed, err := time.Parse(time.RFC3339Nano, gotSince)
	if err != nil || !parsed.Equal(since) {
		t.Fatalf("since = %q (%v)", gotSince, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outcomes = %+v, want %+v", got, want)
	}

	if _, err := client.ImplementationOutcomes(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if gotSince != "" {
		t.Fatalf("a zero since must not be sent, got %q", gotSince)
	}
}

// TestImplementationOutcomesSurfacesTypedRefusals proves a containment refusal
// arrives as itself — a stage must be able to tell "the daemon refused this
// read" from "there is no evidence", which is exactly the confusion that would
// silently de-ready nothing forever.
func TestImplementationOutcomesSurfacesTypedRefusals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(apicontract.ErrorEnvelope{Error: apicontract.APIError{
			Code:    "gaggle_mismatch",
			Message: "pod principal may only read its own gaggle's telemetry",
		}})
	}))
	defer server.Close()

	client, err := NewHTTP(Config{BaseURL: server.URL, Token: "t", Gaggle: "platform"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ImplementationOutcomes(context.Background(), time.Time{})
	var planeErr *Error
	if !errors.As(err, &planeErr) {
		t.Fatalf("error = %v, want a typed plane refusal", err)
	}
	if planeErr.Status != http.StatusForbidden || planeErr.Code != "gaggle_mismatch" {
		t.Fatalf("refusal = %+v", planeErr)
	}
}

// TestNewHTTPRequiresEveryContainmentInput keeps the constructor as strict as
// Select: no base URL, no bearer, no gaggle, no client.
func TestNewHTTPRequiresEveryContainmentInput(t *testing.T) {
	for _, cfg := range []Config{
		{Token: "t", Gaggle: "core"},
		{BaseURL: "https://d", Gaggle: "core"},
		{BaseURL: "https://d", Token: "t"},
	} {
		if _, err := NewHTTP(cfg); err == nil {
			t.Fatalf("NewHTTP(%+v) = nil error, want refusal", cfg)
		}
	}
}
