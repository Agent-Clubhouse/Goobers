package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/stateclient"
)

// fakeStateService records what reached the service and answers whatever the
// test staged, so the route's own behaviour — preconditions, containment,
// status mapping — is what these tests observe.
type fakeStateService struct {
	value    StateValue
	err      error
	gets     []StateGetRequest
	puts     []StatePutRequest
	getErr   error
	podRuns  map[string]string // runID -> the gaggle it belongs to
	contains bool
}

func (f *fakeStateService) authorize(request StateGetRequest) error {
	if !request.PodScoped || !f.contains {
		return nil
	}
	if f.podRuns[request.RunID] != request.Gaggle {
		return NewInterventionError(http.StatusForbidden, "gaggle_mismatch", "not your gaggle", nil)
	}
	return nil
}

func (f *fakeStateService) GetState(_ context.Context, request StateGetRequest) (StateValue, error) {
	f.gets = append(f.gets, request)
	if err := f.authorize(request); err != nil {
		return StateValue{}, err
	}
	if f.getErr != nil {
		return StateValue{}, f.getErr
	}
	return f.value, nil
}

func (f *fakeStateService) PutState(_ context.Context, request StatePutRequest) (StateValue, error) {
	f.puts = append(f.puts, request)
	if err := f.authorize(request.StateGetRequest); err != nil {
		return StateValue{}, err
	}
	if f.err != nil {
		return StateValue{}, f.err
	}
	return StateValue{Data: request.Data, ETag: stateclient.ETagFor(request.Data), Found: true}, nil
}

func statePath(gaggle, key string) string {
	return "/api/v1/gaggles/" + gaggle + "/state/" + key
}

func statePutRequest(gaggle, key, body string, headers map[string]string) *http.Request {
	request := httptest.NewRequest(http.MethodPut, statePath(gaggle, key), strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return request
}

// TestStatePlaneRoutesAreInTheContract pins the route pair onto the §7
// contract discipline: the read half must stay a genuine read (so it is not
// shed as a mutation or classified as workflow execution), and the write half
// must stay a mutation.
func TestStatePlaneRoutesAreInTheContract(t *testing.T) {
	get, ok := apicontract.V1Route(apicontract.RouteGaggleStateGet)
	if !ok {
		t.Fatal("gaggleStateGet is not in the V1 contract")
	}
	put, ok := apicontract.V1Route(apicontract.RouteGaggleStatePut)
	if !ok {
		t.Fatal("gaggleStatePut is not in the V1 contract")
	}
	if get.Method != http.MethodGet || put.Method != http.MethodPut {
		t.Fatalf("methods = %s/%s", get.Method, put.Method)
	}
	if get.Path != apicontract.GaggleStateKeyPath || put.Path != apicontract.GaggleStateKeyPath {
		t.Fatalf("paths = %s/%s, want both on the shared key path", get.Path, put.Path)
	}
	if get.ActionClass != apicontract.ActionReadOnlyNavigation {
		t.Fatalf("GET action class = %q, want read-only-navigation", get.ActionClass)
	}
	if put.ActionClass != apicontract.ActionWorkflowExecution || put.Cost != apicontract.CostMutation {
		t.Fatalf("PUT class/cost = %s/%s, want workflow-execution mutation", put.ActionClass, put.Cost)
	}
	if MaxStateValueBytes != stateclient.MaxValueBytes {
		t.Fatalf("MaxStateValueBytes = %d, want the client's own cap %d", MaxStateValueBytes, stateclient.MaxValueBytes)
	}
}

func TestStateGetServesValueAndETag(t *testing.T) {
	body := []byte(`{"511":{}}`)
	service := &fakeStateService{value: StateValue{Data: body, ETag: stateclient.ETagFor(body), Found: true}}
	handler := writePlaneHandler(t, nil, AllowAll, WithStateService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, statePath("goobers", stateclient.KeyBlockedRecords), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if got := response.Body.String(); got != string(body) {
		t.Fatalf("body = %q, want the stored bytes verbatim", got)
	}
	if got := response.Header().Get("ETag"); got != `"`+stateclient.ETagFor(body)+`"` {
		t.Fatalf("ETag = %q, want the value's quoted digest", got)
	}
	// A cached scheduler-state read would hand a CAS loop an ETag that can
	// never match, spinning it until it exhausts its attempts.
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if len(service.gets) != 1 || service.gets[0].Gaggle != "goobers" || service.gets[0].Key != stateclient.KeyBlockedRecords {
		t.Fatalf("service saw %+v", service.gets)
	}
}

// TestStateGetAbsentKeyIs404 pins the first-run state of every one of these
// files: absent is not an error, and the client turns it back into the zero
// value.
func TestStateGetAbsentKeyIs404(t *testing.T) {
	service := &fakeStateService{value: StateValue{Found: false}}
	handler := writePlaneHandler(t, nil, AllowAll, WithStateService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, statePath("goobers", stateclient.KeySiblingContextCache), nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an absent key", response.Code)
	}
}

// TestStatePlaneRefusesKeysOutsideTheClosedNamespace is the containment case
// that matters most: the plane's bearer must never become a read or a write of
// claims.json, the instance config, or anything else that happens to share the
// scheduler directory — and never a traversal out of it.
func TestStatePlaneRefusesKeysOutsideTheClosedNamespace(t *testing.T) {
	service := &fakeStateService{}
	handler := writePlaneHandler(t, nil, AllowAll, WithStateService(service))

	for _, key := range []string{
		"claims.json",
		"config.yaml",
		"..%2F..%2Fconfig.yaml",
		"blocked.json.tmp",
		"backlog-scan-nothex.json",
		"backlog-scan-" + strings.Repeat("a", 63) + ".json",
	} {
		t.Run(key, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, statePath("goobers", key), nil))
			if response.Code != http.StatusBadRequest && response.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want the key refused", response.Code)
			}
			for _, seen := range service.gets {
				if seen.Key == key {
					t.Fatalf("key %q reached the service", key)
				}
			}
		})
	}
	if len(service.puts) != 0 {
		t.Fatalf("service saw writes for refused keys: %+v", service.puts)
	}
}

// TestStatePutRequiresAPrecondition is the heart of the route: an
// unconditional PUT is exactly the blind overwrite — the lost update — the
// plane exists to make impossible, so it is refused rather than served.
func TestStatePutRequiresAPrecondition(t *testing.T) {
	service := &fakeStateService{}
	handler := writePlaneHandler(t, nil, AllowAll, WithStateService(service))

	cases := []struct {
		name    string
		headers map[string]string
		status  int
	}{
		{"unconditional", nil, http.StatusPreconditionRequired},
		{"both", map[string]string{"If-Match": `"abc"`, "If-None-Match": "*"}, http.StatusBadRequest},
		{"wildcard if-match", map[string]string{"If-Match": "*"}, http.StatusBadRequest},
		{"multi-tag if-match", map[string]string{"If-Match": `"abc", "def"`}, http.StatusBadRequest},
		{"non-wildcard if-none-match", map[string]string{"If-None-Match": `"abc"`}, http.StatusBadRequest},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, statePutRequest("goobers", stateclient.KeyBlockedRecords, "{}", testCase.headers))
			if response.Code != testCase.status {
				t.Fatalf("status = %d, want %d (body %s)", response.Code, testCase.status, response.Body)
			}
		})
	}
	if len(service.puts) != 0 {
		t.Fatalf("a write without a valid precondition reached the service: %+v", service.puts)
	}
}

func TestStatePutCompareAndSwap(t *testing.T) {
	service := &fakeStateService{}
	handler := writePlaneHandler(t, nil, AllowAll, WithStateService(service))

	// Replace an exact version.
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, statePutRequest("goobers", stateclient.KeyBlockedRecords, `{"a":1}`,
		map[string]string{"If-Match": `"deadbeef"`}))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if got := response.Header().Get("ETag"); got != `"`+stateclient.ETagFor([]byte(`{"a":1}`))+`"` {
		t.Fatalf("ETag = %q, want the written value's digest so a session can chain writes", got)
	}
	if len(service.puts) != 1 || service.puts[0].IfMatch != "deadbeef" {
		t.Fatalf("service saw %+v, want the unquoted entity tag", service.puts)
	}

	// Create-if-absent carries an EMPTY IfMatch, which the store reads as
	// "the key must not exist" — the same shape a file backend's absent-file
	// read produces.
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, statePutRequest("goobers", stateclient.KeyBlockedRecords, `{"b":2}`,
		map[string]string{"If-None-Match": "*"}))
	if response.Code != http.StatusNoContent {
		t.Fatalf("create status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.puts) != 2 || service.puts[1].IfMatch != "" {
		t.Fatalf("service saw %+v, want an empty IfMatch for create-if-absent", service.puts)
	}
}

// TestStatePutLostCompareAndSwapIs412 pins the status the client's Update loop
// keys off: a lost swap is a business outcome to retry, never a fault.
func TestStatePutLostCompareAndSwapIs412(t *testing.T) {
	service := &fakeStateService{err: ErrStatePrecondition}
	handler := writePlaneHandler(t, nil, AllowAll, WithStateService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, statePutRequest("goobers", stateclient.KeyBlockedRecords, "{}",
		map[string]string{"If-Match": `"stale"`}))
	if response.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", response.Code)
	}
	if !strings.Contains(response.Body.String(), "precondition_failed") {
		t.Fatalf("body = %s, want the typed precondition code", response.Body)
	}
}

// TestPodPrincipalIsConfinedToItsOwnGaggleState is decision 005 R3's
// containment: a pod may read and compare-and-swap the scheduler state of the
// gaggle its own run belongs to, and nothing else. The route's job is to hand
// the service the caller's own run id; the service's job is to verify it.
func TestPodPrincipalIsConfinedToItsOwnGaggleState(t *testing.T) {
	service := &fakeStateService{
		contains: true,
		podRuns:  map[string]string{"run-1": "goobers"},
		value:    StateValue{Data: []byte("{}"), ETag: stateclient.ETagFor([]byte("{}")), Found: true},
	}
	pod := &fakeAuthenticator{principal: &Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer}}
	handler := writePlaneHandler(t, pod, AllowAll, WithStateService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, statePath("goobers", stateclient.KeyBlockedRecords), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("own-gaggle read status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.gets) != 1 || !service.gets[0].PodScoped || service.gets[0].RunID != "run-1" {
		t.Fatalf("service saw %+v, want the caller's own run bound by the route", service.gets)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, statePath("other", stateclient.KeyBlockedRecords), nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("foreign-gaggle read status = %d, want 403", response.Code)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, statePutRequest("other", stateclient.KeyBlockedRecords, "{}",
		map[string]string{"If-Match": `"abc"`}))
	if response.Code != http.StatusForbidden {
		t.Fatalf("foreign-gaggle write status = %d, want 403", response.Code)
	}
}

// TestStatePlaneRefusesAPodPrincipalWithNoRun fails closed on a pod token that
// does not name a run: there is no gaggle it can be contained to.
func TestStatePlaneRefusesAPodPrincipalWithNoRun(t *testing.T) {
	service := &fakeStateService{}
	pod := &fakeAuthenticator{principal: &Principal{Subject: "run:", Issuer: PodPrincipalIssuer}}
	handler := writePlaneHandler(t, pod, AllowAll, WithStateService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, statePath("goobers", stateclient.KeyBlockedRecords), nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	if len(service.gets) != 0 {
		t.Fatalf("an unbindable pod principal reached the service: %+v", service.gets)
	}
}

// TestStatePlaneUnavailableWithoutService pins the same structured 503 the
// other planes answer, rather than the routes silently not existing.
func TestStatePlaneUnavailableWithoutService(t *testing.T) {
	handler := writePlaneHandler(t, nil, AllowAll)

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, statePath("goobers", stateclient.KeyBlockedRecords), nil),
		statePutRequest("goobers", stateclient.KeyBlockedRecords, "{}", map[string]string{"If-Match": `"abc"`}),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want 503", request.Method, response.Code)
		}
		if !strings.Contains(response.Body.String(), "state_unavailable") {
			t.Fatalf("body = %s, want the typed unavailability code", response.Body)
		}
	}
}

// fakeTriggerCapture records what the trigger route handed the service.
type fakeTriggerCapture struct {
	requests []TriggerRequest
}

func (f *fakeTriggerCapture) Trigger(_ context.Context, request TriggerRequest) (TriggerResponse, error) {
	f.requests = append(f.requests, request)
	return TriggerResponse{RunID: "run-minted"}, nil
}

// TestPodPrincipalTriggersOnlyForItsOwnGaggle is the other half of decision
// 005 R3: a pod may POST /triggers, but only naming its own gaggle and only
// attributing a priority re-tick to its own run. The route binds the caller's
// identity; the daemon service verifies gaggle membership independently
// (cmd/goobers: daemonTriggerService.contains).
func TestPodPrincipalTriggersOnlyForItsOwnGaggle(t *testing.T) {
	service := &fakeTriggerCapture{}
	pod := &fakeAuthenticator{principal: &Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer}}
	handler := writePlaneHandler(t, pod, AllowAll, WithTriggerService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.TriggerIngestPath,
		`{"gaggle":"goobers","workflow":"merge-review","sourceRun":"run-1"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("own-run priority trigger status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.requests) != 1 {
		t.Fatalf("service saw %d requests", len(service.requests))
	}
	got := service.requests[0]
	if !got.PodScoped || got.PodRunID != "run-1" || got.SourceRun != "run-1" {
		t.Fatalf("request = %+v, want pod-scoped and bound to the caller's own run", got)
	}

	// An unscoped trigger from a pod would let the daemon's workflow-name
	// resolution reach into some other gaggle.
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.TriggerIngestPath,
		`{"workflow":"merge-review"}`))
	if response.Code != http.StatusForbidden {
		t.Fatalf("unscoped pod trigger status = %d, want 403", response.Code)
	}

	// Attributing a priority re-tick to somebody else's run would let a pod
	// claim another run's published state as the reason for the mint.
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.TriggerIngestPath,
		`{"gaggle":"goobers","workflow":"merge-review","sourceRun":"run-2"}`))
	if response.Code != http.StatusForbidden {
		t.Fatalf("foreign source-run status = %d, want 403", response.Code)
	}
	if len(service.requests) != 1 {
		t.Fatalf("a refused trigger reached the service: %+v", service.requests)
	}
}

// TestHumanTriggerIsNotPodScoped keeps the containment additive: an operator
// or the portal still triggers by workflow name with no gaggle, exactly as
// before the pod path existed.
func TestHumanTriggerIsNotPodScoped(t *testing.T) {
	service := &fakeTriggerCapture{}
	handler := writePlaneHandler(t, nil, AllowAll, WithTriggerService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.TriggerIngestPath, `{"workflow":"merge-review"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.requests) != 1 || service.requests[0].PodScoped {
		t.Fatalf("request = %+v, want a non-pod trigger", service.requests)
	}
}
