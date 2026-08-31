package telemetryclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// defectaggregates_test.go pins the stage side of the defect-nomination
// aggregate plane (Goobers#4001): what a stage may ASK for, what it refuses to
// TALK to, and what it refuses to BELIEVE.

func testEnv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// TestSelectFailsClosedOnPartialConfiguration is the fail-closed half. Every
// incomplete combination must be an ERROR, never a silent demotion to the
// local path — in a pod that path reads an empty worktree and reports no
// defects at all.
func TestSelectFailsClosedOnPartialConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		selected bool
		wantErr  bool
	}{
		{name: "nothing set stays local", env: nil},
		{
			name:    "endpoint without a token",
			env:     map[string]string{EnvEndpoint: "https://daemon.internal", EnvGaggle: "core"},
			wantErr: true,
		},
		{
			name:    "endpoint and token without a gaggle",
			env:     map[string]string{EnvEndpoint: "https://daemon.internal", EnvToken: "t"},
			wantErr: true,
		},
		{
			name:    "fallback endpoint without its token",
			env:     map[string]string{EnvFallbackEndpoint: "https://daemon.internal", EnvGaggle: "core"},
			wantErr: true,
		},
		{
			// The telemetry bearer and the pod control-plane bearer are
			// separate credentials. A telemetry endpoint paired with only the
			// control-plane token must not silently borrow it.
			name:    "telemetry endpoint does not borrow the pod token",
			env:     map[string]string{EnvEndpoint: "https://daemon.internal", EnvFallbackToken: "pod", EnvGaggle: "core"},
			wantErr: true,
		},
		{
			name:     "complete telemetry configuration",
			env:      map[string]string{EnvEndpoint: "https://daemon.internal", EnvToken: "t", EnvGaggle: "core"},
			selected: true,
		},
		{
			name:     "complete fallback configuration",
			env:      map[string]string{EnvFallbackEndpoint: "https://daemon.internal", EnvFallbackToken: "p", EnvGaggle: "core"},
			selected: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, selected, err := Select(testEnv(test.env))
			if test.wantErr {
				if err == nil {
					t.Fatalf("Select() = %v, %v, nil; want an error", client, selected)
				}
				if selected {
					t.Fatal("a failed selection must not report the plane as selected")
				}
				return
			}
			if err != nil {
				t.Fatalf("Select() error = %v", err)
			}
			if selected != test.selected {
				t.Fatalf("selected = %v, want %v", selected, test.selected)
			}
		})
	}
}

// TestSelectRefusesHostileEndpoints is the SSRF half. The endpoint arrives
// from the pod's environment, which the dispatcher stamps — but a stage that
// can influence its own environment must not be able to point the read at
// something else, or at a credential-leaking URL shape.
func TestSelectRefusesHostileEndpoints(t *testing.T) {
	hostile := []string{
		"file:///etc/passwd",
		"gopher://127.0.0.1:11211/",
		"ftp://daemon.internal",
		"unix:///var/run/docker.sock",
		"//daemon.internal",
		"daemon.internal",
		"https://",
		"http://",
		"https://user:secret@daemon.internal",
		"https://daemon.internal/api?token=leak",
		"https://daemon.internal/api#fragment",
		"https://daemon.internal/../../etc",
		"https://daemon.internal/a/../../b",
	}
	for _, endpoint := range hostile {
		t.Run(endpoint, func(t *testing.T) {
			if _, _, err := Select(testEnv(map[string]string{
				EnvEndpoint: endpoint, EnvToken: "t", EnvGaggle: "core",
			})); err == nil {
				t.Fatalf("endpoint %q was accepted", endpoint)
			}
		})
	}
	for _, endpoint := range []string{
		"https://daemon.internal",
		"http://127.0.0.1:8080",
		"https://daemon.internal/base",
	} {
		t.Run("admitted/"+endpoint, func(t *testing.T) {
			if _, _, err := Select(testEnv(map[string]string{
				EnvEndpoint: endpoint, EnvToken: "t", EnvGaggle: "core",
			})); err != nil {
				t.Fatalf("endpoint %q was refused: %v", endpoint, err)
			}
		})
	}
}

// TestSelectRefusesHostileGaggleNames pins the traversal half at the point
// the name ENTERS the client, so no later caller has to remember to check it.
func TestSelectRefusesHostileGaggleNames(t *testing.T) {
	for _, gaggle := range []string{
		"../../etc", "..", "core/../platform", "core/sub", "/core", "core name",
		"core\x00", strings.Repeat("g", 200), "-core",
	} {
		t.Run(gaggle, func(t *testing.T) {
			if _, _, err := Select(testEnv(map[string]string{
				EnvEndpoint: "https://daemon.internal", EnvToken: "t", EnvGaggle: gaggle,
			})); err == nil {
				t.Fatalf("gaggle %q was accepted", gaggle)
			}
		})
	}
}

// TestNormalizeErrorSignatureRedactsAnythingUnsafe is decision 005 R4's line,
// tested as a table. An identifier-shaped classification survives verbatim —
// that is what preserves the CLI's output semantics for real rollup data —
// and anything that could carry a message, a path, a URL, an address or a
// token is replaced by an opaque digest that still CLUSTERS.
func TestNormalizeErrorSignatureRedactsAnythingUnsafe(t *testing.T) {
	safe := []string{
		"timeout", "exit_status_1", "GATE_FAILED", "http.502",
		"provider:rate-limited", "gh-api+retry", "a",
	}
	for _, code := range safe {
		t.Run("safe/"+code, func(t *testing.T) {
			subject, signature := NormalizeErrorSignature(code)
			if subject != code {
				t.Fatalf("subject = %q, want the code unchanged", subject)
			}
			if !strings.HasPrefix(signature, ErrorSignaturePrefix) {
				t.Fatalf("signature = %q, want the %s digest", signature, ErrorSignaturePrefix)
			}
		})
	}
	hostile := []string{
		"failed to open /Users/alice/.config/goobers/token.json",
		"dial tcp 10.1.2.3:5432: connect: connection refused",
		"https://api.github.com/repos/acme/web/issues/42",
		`unexpected token "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"`,
		"panic: runtime error: index out of range [3] with length 2",
		"",
		strings.Repeat("x", 500),
		"has spaces",
		"9-leading-digit",
	}
	for _, code := range hostile {
		t.Run("hostile/"+code, func(t *testing.T) {
			subject, signature := NormalizeErrorSignature(code)
			if strings.HasPrefix(subject, RedactedSignatureSubject) == false {
				t.Fatalf("subject = %q, want a %s form", subject, RedactedSignatureSubject)
			}
			if code != "" && strings.Contains(subject+signature, code) {
				t.Fatalf("the raw code survived normalization: %q / %q", subject, signature)
			}
			for _, leak := range []string{"/Users/", "10.1.2.3", "api.github.com", "ghp_", " "} {
				if strings.Contains(subject, leak) || strings.Contains(signature, leak) {
					t.Fatalf("normalized form leaks %q: %q / %q", leak, subject, signature)
				}
			}
		})
	}
	t.Run("the same hostile code always clusters", func(t *testing.T) {
		code := "dial tcp 10.1.2.3:5432: connect: connection refused"
		firstSubject, firstSignature := NormalizeErrorSignature(code)
		secondSubject, secondSignature := NormalizeErrorSignature(code)
		if firstSubject != secondSubject || firstSignature != secondSignature {
			t.Fatal("normalization is not stable, so a nomination lane cannot dedupe on it")
		}
		otherSubject, _ := NormalizeErrorSignature("dial tcp 10.9.9.9:5432: connect: connection refused")
		if otherSubject == firstSubject {
			t.Fatal("two different codes collapsed onto one subject")
		}
	})
}

// TestDefectAggregateQueryCarriesOnlyTheClosedParameterSet pins that the
// client cannot express a path, a projection or a query fragment: the request
// type has no field for one, and the rendered query is the closed set.
func TestDefectAggregateQueryCarriesOnlyTheClosedParameterSet(t *testing.T) {
	since := time.Now().UTC().Add(-time.Hour)
	values, err := DefectAggregateQuery(DefectAggregateRequest{
		Gaggle:     "core",
		Workflow:   "nomination",
		Since:      since,
		Aggregates: []Aggregate{AggregateGateNoise, AggregateStageFailureRate},
		Thresholds: Thresholds{MinSamples: 7, MaxFailureRate: 0.4},
	})
	if err != nil {
		t.Fatal(err)
	}
	admitted := map[string]bool{
		"gaggle": true, "workflow": true, "since": true, "aggregates": true,
		"minSamples": true, "maxFailureRate": true, "minErrorSignatureCount": true,
		"minGateEvaluations": true, "maxGateEscalationRate": true, "maxFlaggedRuns": true,
		"minCreditRuns": true, "minCreditFailureShare": true,
	}
	for name := range values {
		if !admitted[name] {
			t.Fatalf("query carries an unadmitted parameter %q", name)
		}
		if len(values[name]) != 1 {
			t.Fatalf("parameter %q was repeated, which the server refuses", name)
		}
	}
	if values.Get("aggregates") != "gate-noise,stage-failure-rate" {
		t.Fatalf("aggregates = %q", values.Get("aggregates"))
	}
	if values.Get("since") != since.Format(time.RFC3339Nano) {
		t.Fatalf("since = %q, want %q", values.Get("since"), since.Format(time.RFC3339Nano))
	}
}

// TestDefectAggregatesRefusesAForeignGaggleBeforeSending is the cross-gaggle
// half on the client side. The daemon refuses it too; refusing here as well
// means a lane cannot even ATTEMPT a read outside its own containment.
func TestDefectAggregatesRefusesAForeignGaggleBeforeSending(t *testing.T) {
	reached := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer server.Close()
	client, err := NewHTTP(Config{BaseURL: server.URL, Token: "t", Gaggle: "core"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.DefectAggregates(context.Background(), DefectAggregateRequest{
		Gaggle: "platform",
		Since:  time.Now().UTC().Add(-time.Hour),
	})
	if err == nil {
		t.Fatal("a foreign gaggle read was attempted")
	}
	if reached {
		t.Fatal("the request reached the server")
	}
}

// TestDefectAggregatesBoundsWhatItBelieves pins the response-side ceilings. A
// stage pod must not be exhausted, or misled, by whatever answers the
// endpoint it was handed.
func TestDefectAggregatesBoundsWhatItBelieves(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "an oversized body is refused",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"gaggle":"core","note":"`))
				chunk := strings.Repeat("A", 1<<16)
				for written := 0; written < (9 << 20); written += len(chunk) {
					if _, err := w.Write([]byte(chunk)); err != nil {
						return
					}
				}
				_, _ = w.Write([]byte(`"}`))
			},
		},
		{
			name: "an answer for another gaggle is refused",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeTestJSON(w, DefectAggregateResponse{Gaggle: "platform"})
			},
		},
		{
			name: "too many findings are refused",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				response := DefectAggregateResponse{Gaggle: "core"}
				for i := 0; i < MaxFindings+1; i++ {
					response.Findings = append(response.Findings,
						Finding{Kind: "stage-failure-rate", Subject: fmt.Sprintf("s%d", i)})
				}
				writeTestJSON(w, response)
			},
		},
		{
			name: "a non-200 is refused",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":"gaggle_mismatch","message":"refused"}}`))
			},
		},
		{
			name: "a non-JSON body is refused",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("<html>not json</html>"))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			client, err := NewHTTP(Config{BaseURL: server.URL, Token: "t", Gaggle: "core"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.DefectAggregates(context.Background(), DefectAggregateRequest{
				Since: time.Now().UTC().Add(-time.Hour),
			}); err == nil {
				t.Fatal("the answer was believed")
			}
		})
	}
}

// TestDefectAggregatesRefusesAnUnboundedWindow pins that neither side will
// walk the whole rollup, whatever the caller asks.
func TestDefectAggregatesRefusesAnUnboundedWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, DefectAggregateResponse{Gaggle: "core"})
	}))
	defer server.Close()
	client, err := NewHTTP(Config{BaseURL: server.URL, Token: "t", Gaggle: "core"})
	if err != nil {
		t.Fatal(err)
	}
	for _, since := range []time.Time{
		{},
		time.Now().UTC().Add(-MaxWindow - time.Hour),
		time.Now().UTC().Add(2 * time.Hour),
	} {
		if _, err := client.DefectAggregates(context.Background(),
			DefectAggregateRequest{Since: since}); err == nil {
			t.Fatalf("since %s was accepted", since)
		}
	}
}

// TestDefectAggregatesSendsItsOwnBearerAndScope pins the wire itself: the
// telemetry bearer, the client's own gaggle, and nothing else.
func TestDefectAggregatesSendsItsOwnBearerAndScope(t *testing.T) {
	var seen *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seen = request.Clone(context.Background())
		writeTestJSON(w, DefectAggregateResponse{Gaggle: "core", Findings: []Finding{}})
	}))
	defer server.Close()
	client, err := NewHTTP(Config{BaseURL: server.URL, Token: "telemetry-bearer", Gaggle: "core"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DefectAggregates(context.Background(), DefectAggregateRequest{
		Since:      time.Now().UTC().Add(-time.Hour),
		Aggregates: []Aggregate{AggregateErrorSignature},
	}); err != nil {
		t.Fatal(err)
	}
	if seen.Method != http.MethodGet {
		t.Fatalf("method = %s", seen.Method)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer telemetry-bearer" {
		t.Fatalf("authorization = %q", got)
	}
	if seen.URL.Query().Get("gaggle") != "core" {
		t.Fatalf("gaggle = %q", seen.URL.Query().Get("gaggle"))
	}
	if seen.ContentLength > 0 {
		t.Fatal("a read carried a body")
	}
}

// TestParseAggregateAdmitsOnlyTheRuledFour pins the admitted set itself.
func TestParseAggregateAdmitsOnlyTheRuledFour(t *testing.T) {
	for _, name := range []string{"stage-failure-rate", "error-signature", "gate-noise", "credit-assignment"} {
		if _, err := ParseAggregate(name); err != nil {
			t.Fatalf("%q was refused: %v", name, err)
		}
	}
	for _, name := range []string{
		"all", "ci-check-failure", "workflow-untriggered", "stage-unreached",
		"learning-episode", "", "  ", "Error-Signature", "stage-failure-rate;drop",
	} {
		if _, err := ParseAggregate(name); err == nil {
			t.Fatalf("%q was admitted", name)
		}
	}
	// Surrounding whitespace is trimmed rather than refused: a comma-separated
	// list written `a, b` is an ordinary spelling, not an attack.
	if _, err := ParseAggregate("  gate-noise  "); err != nil {
		t.Fatalf("a padded name was refused: %v", err)
	}
	if len(AdmittedAggregates()) != 4 {
		t.Fatalf("admitted set = %v, want exactly the ruled four", AdmittedAggregates())
	}
	// The slice must be a copy: a caller that mutates it must not be able to
	// widen the plane for everyone else in the process.
	admitted := AdmittedAggregates()
	admitted[0] = "all"
	if AdmittedAggregates()[0] == "all" {
		t.Fatal("AdmittedAggregates() shares its backing array")
	}
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
