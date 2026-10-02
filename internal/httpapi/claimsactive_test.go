package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

type activeClaimsReader struct {
	*fakeReader
	list  readservice.ActiveClaimList
	reads int
}

func (r *activeClaimsReader) ActiveClaims(context.Context) (readservice.ActiveClaimList, error) {
	r.reads++
	return r.list, nil
}

func TestActiveClaimsRouteServesViewersOnly(t *testing.T) {
	claimedAt := time.Date(2026, 10, 2, 5, 0, 0, 0, time.UTC)
	list := readservice.ActiveClaimList{
		ObservedAt: claimedAt.Add(time.Hour),
		Claims: []readservice.ActiveClaim{{
			ItemID: "1488", Workflow: "implement", RunID: "run-a", Holder: readservice.ActiveClaimHolderLocal,
			ClaimedAt: claimedAt, ExpiresAt: claimedAt.Add(2 * time.Hour), AgeSeconds: 3600,
		}},
	}
	for _, tc := range []struct {
		name      string
		principal *Principal
		status    int
	}{
		{"viewer", &Principal{Subject: "viewer", Roles: []Role{RoleView}}, http.StatusOK},
		{"no role", &Principal{Subject: "unprivileged"}, http.StatusForbidden},
		{"pod", &Principal{Subject: "run:other", Issuer: PodPrincipalIssuer, Roles: []Role{RoleView}}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &activeClaimsReader{fakeReader: &fakeReader{}, list: list}
			handler, err := NewHandler(reader, RequireRoles(), discardLogger(), WithAuthenticator(&fakeAuthenticator{principal: tc.principal}))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.ClaimsActivePath, nil))
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tc.status != http.StatusOK {
				if reader.reads != 0 {
					t.Fatalf("unauthorized request reached reader %d times", reader.reads)
				}
				return
			}
			var got readservice.ActiveClaimList
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Claims) != 1 || got.Claims[0] != list.Claims[0] {
				t.Fatalf("claims = %+v, want %+v", got.Claims, list.Claims)
			}
		})
	}
}

func TestActiveClaimsRouteIsUnconfiguredWithoutTheReadSurface(t *testing.T) {
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(),
		WithDiscoveryIdentity(DiscoveryIdentity{DaemonInstanceID: "instance-1", DaemonBootID: "b7fc610d-3c79-4fd4-9bb3-da569b6d14d5"}))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.ClaimsActivePath, nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	capabilities := httptest.NewRecorder()
	handler.ServeHTTP(capabilities, httptest.NewRequest(http.MethodGet, apicontract.CapabilitiesPath, nil))
	var document apicontract.CapabilityDocument
	if err := json.NewDecoder(capabilities.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	if capability := capabilityByID(t, document, apicontract.RouteClaimsActive); capability.Available {
		t.Fatalf("capability = %+v, want unavailable", capability)
	}
}
