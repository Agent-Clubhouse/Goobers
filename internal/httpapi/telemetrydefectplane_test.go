package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/telemetryclient"
)

// telemetrydefectplane_test.go pins decision 005 R4 AS AMENDED by
// Goobers#4001. Every test here is a line the amendment must not be allowed to
// erase later: the plane is bearer-separated, gaggle-contained, closed over
// its parameters, bounded in every dimension, and it never carries a raw
// error signature.

type fakeDefectService struct {
	response telemetryclient.DefectAggregateResponse
	err      error
	request  TelemetryDefectAggregateRequest
	calls    int
}

func (f *fakeDefectService) DefectAggregates(
	_ context.Context,
	request TelemetryDefectAggregateRequest,
) (telemetryclient.DefectAggregateResponse, error) {
	f.calls++
	f.request = request
	return f.response, f.err
}

func defectHandler(t *testing.T, service TelemetryDefectAggregateService, options ...HandlerOption) http.Handler {
	t.Helper()
	if service != nil {
		options = append(options, WithTelemetryDefectAggregateService(service))
	}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), options...)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func defectQuery(extra string) string {
	query := "?gaggle=core&aggregates=stage-failure-rate&since=" +
		time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339)
	if extra != "" {
		query += "&" + extra
	}
	return query
}

// TestDefectAggregateRouteIsBearerSeparated pins the authorizer half. The
// amendment admitted ONE new path for pod principals; it did not open the
// error-signature route it sits beside, and it did not open a method other
// than GET.
func TestDefectAggregateRouteIsBearerSeparated(t *testing.T) {
	authorizer := RequireRoles()
	tests := []struct {
		name      string
		principal Principal
		method    string
		path      string
		allowed   bool
	}{
		{
			name:      "pod GET admitted",
			principal: Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer},
			method:    http.MethodGet,
			path:      apicontract.TelemetryDefectAggregatesPath,
			allowed:   true,
		},
		{
			name:      "pod POST refused",
			principal: Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer},
			method:    http.MethodPost,
			path:      apicontract.TelemetryDefectAggregatesPath,
			allowed:   false,
		},
		{
			name:      "pod HEAD refused",
			principal: Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer},
			method:    http.MethodHead,
			path:      apicontract.TelemetryDefectAggregatesPath,
			allowed:   false,
		},
		{
			// The raw (code, error_class) route is what R4 keeps off the
			// plane. Admitting the derived aggregate must not admit it.
			name:      "raw error signatures still refused for pods",
			principal: Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer},
			method:    http.MethodGet,
			path:      TelemetryErrorSignaturesPath,
			allowed:   false,
		},
		{
			name:      "a viewer role reaches it too",
			principal: Principal{Subject: "viewer", Roles: []Role{RoleView}},
			method:    http.MethodGet,
			path:      apicontract.TelemetryDefectAggregatesPath,
			allowed:   true,
		},
		{
			name:      "a principal with no roles is refused",
			principal: Principal{Subject: "nobody"},
			method:    http.MethodGet,
			path:      apicontract.TelemetryDefectAggregatesPath,
			allowed:   false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, test.principal))
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

// TestDefectAggregateReadIsContainedToItsOwnGaggle is the cross-gaggle half.
// A pod may read its own gaggle's aggregates and nothing else, every branch
// that cannot PROVE containment answers 403, and no refusal names a gaggle.
func TestDefectAggregateReadIsContainedToItsOwnGaggle(t *testing.T) {
	tests := []struct {
		name     string
		resolver func(context.Context, string) (string, error)
		gaggle   string
		status   int
		code     string
	}{
		{
			name:     "own gaggle admitted",
			resolver: gaggleResolver(map[string]string{"run-1": "core"}, nil),
			gaggle:   "core",
			status:   http.StatusOK,
		},
		{
			name:     "another gaggle refused",
			resolver: gaggleResolver(map[string]string{"run-1": "core"}, nil),
			gaggle:   "platform",
			status:   http.StatusForbidden,
			code:     "gaggle_mismatch",
		},
		{
			name:     "unresolvable run refused",
			resolver: gaggleResolver(map[string]string{"run-9": "core"}, nil),
			gaggle:   "core",
			status:   http.StatusForbidden,
			code:     "gaggle_mismatch",
		},
		{
			name:     "resolver failure refused",
			resolver: gaggleResolver(nil, errors.New("journal unreadable")),
			gaggle:   "core",
			status:   http.StatusForbidden,
			code:     "gaggle_mismatch",
		},
		{
			name:     "ungaggled run refused",
			resolver: gaggleResolver(map[string]string{"run-1": ""}, nil),
			gaggle:   "core",
			status:   http.StatusForbidden,
			code:     "gaggle_mismatch",
		},
		{
			// Partial configuration fails CLOSED: a daemon with no way to
			// resolve the caller's gaggle serves nobody rather than serving
			// everybody.
			name:     "no resolver wired refuses the whole plane",
			resolver: nil,
			gaggle:   "core",
			status:   http.StatusForbidden,
			code:     "telemetry_scope_unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeDefectService{}
			options := []HandlerOption{WithAuthenticator(podPrincipalFor("run-1"))}
			if test.resolver != nil {
				options = append(options, WithPodRunGaggle(test.resolver))
			}
			handler := defectHandler(t, service, options...)
			response := httptest.NewRecorder()
			target := apicontract.TelemetryDefectAggregatesPath +
				"?gaggle=" + test.gaggle + "&since=" + time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.status, response.Body)
			}
			if test.status != http.StatusOK && service.calls != 0 {
				t.Fatal("a refused read still reached the derivation")
			}
			if test.code == "" {
				return
			}
			var envelope ErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != test.code {
				t.Fatalf("error = %+v, want code %q", envelope.Error, test.code)
			}
			if message := envelope.Error.Message; strings.Contains(message, "core") || strings.Contains(message, "platform") {
				t.Fatalf("refusal message leaks a gaggle name: %q", message)
			}
		})
	}
}

// TestDefectAggregateQuerySurfaceIsClosed is the "clients must not submit
// arbitrary requests" half. The parameter set is an allowlist: a path, a SQL
// fragment, a connector name or a projection selector cannot be smuggled in
// under a key the handler happens not to read.
func TestDefectAggregateQuerySurfaceIsClosed(t *testing.T) {
	since := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	tests := []struct {
		name   string
		query  string
		status int
	}{
		{name: "admitted parameters", query: defectQuery("workflow=nomination"), status: http.StatusOK},
		{name: "unknown parameter refused", query: defectQuery("sql=select+1"), status: http.StatusBadRequest},
		{name: "path parameter refused", query: defectQuery("path=%2Fetc%2Fpasswd"), status: http.StatusBadRequest},
		{name: "connector parameter refused", query: defectQuery("connector=datadog"), status: http.StatusBadRequest},
		{name: "format parameter refused", query: defectQuery("format=tutor-live-verification"), status: http.StatusBadRequest},
		{name: "learning action parameter refused", query: defectQuery("learningAction=code-issue"), status: http.StatusBadRequest},
		{name: "repeated parameter refused", query: defectQuery("gaggle=other"), status: http.StatusBadRequest},
		{name: "missing gaggle refused", query: "?since=" + since, status: http.StatusForbidden},
		{name: "missing since refused", query: "?gaggle=core", status: http.StatusBadRequest},
		{name: "malformed since refused", query: "?gaggle=core&since=yesterday", status: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeDefectService{}
			handler := defectHandler(t, service,
				WithAuthenticator(podPrincipalFor("run-1")),
				WithPodRunGaggle(gaggleResolver(map[string]string{"run-1": "core"}, nil)))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(
				http.MethodGet, apicontract.TelemetryDefectAggregatesPath+test.query, nil))
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.status, response.Body)
			}
			if test.status != http.StatusOK && service.calls != 0 {
				t.Fatal("a rejected query still reached the derivation")
			}
		})
	}
}

// TestDefectAggregateScopeNamesRejectTraversal pins the path/traversal half.
// Gaggle and workflow are FILTERS, not path segments — but they are used to
// select an instance's own scoped state, so a name that could escape one is
// refused at the edge rather than sanitized deeper in.
func TestDefectAggregateScopeNamesRejectTraversal(t *testing.T) {
	hostile := []string{
		"../../etc/passwd",
		"..",
		"core/../platform",
		"core/sub",
		`core\..\platform`,
		"core%2F..%2Fplatform",
		"/absolute",
		"core name",
		strings.Repeat("g", 200),
	}
	for _, name := range hostile {
		t.Run(name, func(t *testing.T) {
			for _, parameter := range []string{"gaggle", "workflow"} {
				service := &fakeDefectService{}
				// The resolver answers whatever the caller claimed, so this
				// test isolates the NAME check from the containment check:
				// containment alone would happily pass a traversal string
				// through.
				handler := defectHandler(t, service,
					WithAuthenticator(podPrincipalFor("run-1")),
					WithPodRunGaggle(func(context.Context, string) (string, error) { return name, nil }))
				query := url.Values{}
				if parameter == "workflow" {
					query.Set("gaggle", "core")
					query.Set("workflow", name)
					handler = defectHandler(t, service,
						WithAuthenticator(podPrincipalFor("run-1")),
						WithPodRunGaggle(gaggleResolver(map[string]string{"run-1": "core"}, nil)))
				} else {
					query.Set("gaggle", name)
				}
				query.Set("since", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(
					http.MethodGet, apicontract.TelemetryDefectAggregatesPath+"?"+query.Encode(), nil))
				if response.Code == http.StatusOK {
					t.Fatalf("%s=%q was admitted", parameter, name)
				}
				if service.calls != 0 {
					t.Fatalf("%s=%q reached the derivation", parameter, name)
				}
			}
		})
	}
}

// TestDefectAggregateAdmittedFamiliesOnly pins the ruling's admitted set. A
// family outside it is REFUSED rather than dropped: a lane that asked for one
// and silently got fewer would file fewer defects and never learn why.
func TestDefectAggregateAdmittedFamiliesOnly(t *testing.T) {
	admitted := []string{"stage-failure-rate", "gate-noise", "credit-assignment", "error-signature"}
	refused := []string{"all", "ci-check-failure", "workflow-untriggered", "stage-unreached", "learning-episode", "", "STAGE-FAILURE-RATE"}
	for _, name := range admitted {
		t.Run("admitted/"+name, func(t *testing.T) {
			service := &fakeDefectService{}
			handler := defectHandler(t, service,
				WithAuthenticator(podPrincipalFor("run-1")),
				WithPodRunGaggle(gaggleResolver(map[string]string{"run-1": "core"}, nil)))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
				apicontract.TelemetryDefectAggregatesPath+"?gaggle=core&aggregates="+name+
					"&since="+time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), nil))
			if response.Code != http.StatusOK {
				t.Fatalf("%s status = %d, body = %s", name, response.Code, response.Body)
			}
			if len(service.request.Aggregates) != 1 || string(service.request.Aggregates[0]) != name {
				t.Fatalf("%s reached the derivation as %v", name, service.request.Aggregates)
			}
		})
	}
	for _, name := range refused {
		t.Run("refused/"+name, func(t *testing.T) {
			service := &fakeDefectService{}
			handler := defectHandler(t, service,
				WithAuthenticator(podPrincipalFor("run-1")),
				WithPodRunGaggle(gaggleResolver(map[string]string{"run-1": "core"}, nil)))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
				apicontract.TelemetryDefectAggregatesPath+"?gaggle=core&aggregates="+name+
					"&since="+time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("%q status = %d, want 400, body = %s", name, response.Code, response.Body)
			}
			if service.calls != 0 {
				t.Fatalf("%q reached the derivation", name)
			}
		})
	}
	t.Run("omitted means the admitted four", func(t *testing.T) {
		service := &fakeDefectService{}
		handler := defectHandler(t, service,
			WithAuthenticator(podPrincipalFor("run-1")),
			WithPodRunGaggle(gaggleResolver(map[string]string{"run-1": "core"}, nil)))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
			apicontract.TelemetryDefectAggregatesPath+"?gaggle=core&since="+
				time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
		if len(service.request.Aggregates) != len(telemetryclient.AdmittedAggregates()) {
			t.Fatalf("aggregates = %v, want the admitted four", service.request.Aggregates)
		}
	})
}

// TestDefectAggregateBoundsAreEnforced pins the window and threshold ceilings.
// Every one of them exists so a single request cannot turn into an unbounded
// walk of the rollup.
func TestDefectAggregateBoundsAreEnforced(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name   string
		query  string
		status int
	}{
		{name: "window at the ceiling", query: "since=" + now.Add(-telemetryclient.MaxWindow+time.Minute).Format(time.RFC3339), status: http.StatusOK},
		{name: "window past the ceiling", query: "since=" + now.Add(-telemetryclient.MaxWindow-time.Hour).Format(time.RFC3339), status: http.StatusBadRequest},
		{name: "since in the future", query: "since=" + now.Add(time.Hour).Format(time.RFC3339), status: http.StatusBadRequest},
		{name: "since within clock skew", query: "since=" + now.Add(time.Minute).Format(time.RFC3339), status: http.StatusOK},
		{name: "minSamples in range", query: "since=" + now.Add(-time.Hour).Format(time.RFC3339) + "&minSamples=7", status: http.StatusOK},
		{name: "minSamples zero", query: "since=" + now.Add(-time.Hour).Format(time.RFC3339) + "&minSamples=0", status: http.StatusBadRequest},
		{name: "minSamples negative", query: "since=" + now.Add(-time.Hour).Format(time.RFC3339) + "&minSamples=-1", status: http.StatusBadRequest},
		{name: "minSamples above the ceiling", query: "since=" + now.Add(-time.Hour).Format(time.RFC3339) + "&minSamples=1000000", status: http.StatusBadRequest},
		{name: "minSamples not a number", query: "since=" + now.Add(-time.Hour).Format(time.RFC3339) + "&minSamples=lots", status: http.StatusBadRequest},
		{name: "maxFailureRate in range", query: "since=" + now.Add(-time.Hour).Format(time.RFC3339) + "&maxFailureRate=0.5", status: http.StatusOK},
		{name: "maxFailureRate above one", query: "since=" + now.Add(-time.Hour).Format(time.RFC3339) + "&maxFailureRate=2", status: http.StatusBadRequest},
		{name: "maxFailureRate NaN", query: "since=" + now.Add(-time.Hour).Format(time.RFC3339) + "&maxFailureRate=NaN", status: http.StatusBadRequest},
		{name: "maxFailureRate Inf", query: "since=" + now.Add(-time.Hour).Format(time.RFC3339) + "&maxFailureRate=Inf", status: http.StatusBadRequest},
		{name: "maxFlaggedRuns above the ceiling", query: "since=" + now.Add(-time.Hour).Format(time.RFC3339) + "&maxFlaggedRuns=100000", status: http.StatusBadRequest},
		{name: "maxFlaggedRuns at the ceiling", query: fmt.Sprintf("since=%s&maxFlaggedRuns=%d", now.Add(-time.Hour).Format(time.RFC3339), telemetryclient.MaxFlaggedRuns), status: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeDefectService{}
			handler := defectHandler(t, service,
				WithAuthenticator(podPrincipalFor("run-1")),
				WithPodRunGaggle(gaggleResolver(map[string]string{"run-1": "core"}, nil)))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
				apicontract.TelemetryDefectAggregatesPath+"?gaggle=core&"+test.query, nil))
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.status, response.Body)
			}
		})
	}
}

// TestDefectAggregateResponseIsBounded pins the cardinality ceiling and, more
// importantly, that hitting it is LOUD. A silently shortened list makes a
// nomination lane under-report, which is the same silent-wrong-result class
// the dispatch refusal existed to prevent.
func TestDefectAggregateResponseIsBounded(t *testing.T) {
	oversized := telemetryclient.DefectAggregateResponse{}
	for i := 0; i < telemetryclient.MaxFindings+25; i++ {
		finding := telemetryclient.Finding{Kind: "stage-failure-rate", Subject: fmt.Sprintf("stage-%d", i)}
		for j := 0; j < telemetryclient.MaxFlaggedRuns+5; j++ {
			finding.FlaggedRuns = append(finding.FlaggedRuns, telemetryclient.JournalPointer{RunID: fmt.Sprintf("run-%d", j)})
		}
		oversized.Findings = append(oversized.Findings, finding)
	}
	for i := 0; i < telemetryclient.MaxCausalEstimates+10; i++ {
		oversized.CausalCredit = append(oversized.CausalCredit, telemetryclient.CausalNodeCredit{Node: fmt.Sprintf("node-%d", i)})
		oversized.PromotionCandidates = append(oversized.PromotionCandidates, telemetryclient.PromotionSignal{Node: fmt.Sprintf("node-%d", i)})
	}
	handler := defectHandler(t, &fakeDefectService{response: oversized},
		WithAuthenticator(podPrincipalFor("run-1")),
		WithPodRunGaggle(gaggleResolver(map[string]string{"run-1": "core"}, nil)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		apicontract.TelemetryDefectAggregatesPath+defectQuery(""), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var decoded telemetryclient.DefectAggregateResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Findings) != telemetryclient.MaxFindings {
		t.Fatalf("findings = %d, want %d", len(decoded.Findings), telemetryclient.MaxFindings)
	}
	if len(decoded.Findings[0].FlaggedRuns) != telemetryclient.MaxFlaggedRuns {
		t.Fatalf("flagged runs = %d, want %d", len(decoded.Findings[0].FlaggedRuns), telemetryclient.MaxFlaggedRuns)
	}
	if len(decoded.CausalCredit) != telemetryclient.MaxCausalEstimates ||
		len(decoded.PromotionCandidates) != telemetryclient.MaxCausalEstimates {
		t.Fatalf("causal credit = %d, promotion candidates = %d",
			len(decoded.CausalCredit), len(decoded.PromotionCandidates))
	}
	if !decoded.Truncated || decoded.Note == "" {
		t.Fatalf("truncation was silent: %+v", decoded)
	}
}

// TestDefectAggregatePlaneFailsClosedWhenUnwired pins the partial-config half
// on the server side: a daemon built without the derivation refuses rather
// than answering "no findings", which a nomination lane would read as "nothing
// is wrong".
func TestDefectAggregatePlaneFailsClosedWhenUnwired(t *testing.T) {
	handler := defectHandler(t, nil,
		WithAuthenticator(podPrincipalFor("run-1")),
		WithPodRunGaggle(gaggleResolver(map[string]string{"run-1": "core"}, nil)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		apicontract.TelemetryDefectAggregatesPath+defectQuery(""), nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", response.Code, response.Body)
	}
	if err := WithTelemetryDefectAggregateService(nil)(&handlerConfig{}); err == nil {
		t.Fatal("WithTelemetryDefectAggregateService(nil) was accepted")
	}
}

// TestDefectAggregateAnswerRestatesItsOwnScope pins that the answer names the
// gaggle, window and families it was derived for. A client that cannot check
// what it was answered cannot detect a misrouted or stale response.
func TestDefectAggregateAnswerRestatesItsOwnScope(t *testing.T) {
	since := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	handler := defectHandler(t, &fakeDefectService{},
		WithAuthenticator(podPrincipalFor("run-1")),
		WithPodRunGaggle(gaggleResolver(map[string]string{"run-1": "core"}, nil)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		apicontract.TelemetryDefectAggregatesPath+
			"?gaggle=core&workflow=nomination&aggregates=gate-noise,credit-assignment&since="+
			since.Format(time.RFC3339), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var decoded telemetryclient.DefectAggregateResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Gaggle != "core" || decoded.Workflow != "nomination" {
		t.Fatalf("scope = %q/%q", decoded.Gaggle, decoded.Workflow)
	}
	if !decoded.Since.Equal(since) {
		t.Fatalf("since = %s, want %s", decoded.Since, since)
	}
	if strings.Join(decoded.Aggregates, ",") != "gate-noise,credit-assignment" {
		t.Fatalf("aggregates = %v", decoded.Aggregates)
	}
	if decoded.Findings == nil || decoded.PromotionCandidates == nil {
		t.Fatal("an empty answer must carry empty arrays, not nulls")
	}
}
