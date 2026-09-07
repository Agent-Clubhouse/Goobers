package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// securityAlertServer is a minimal GitHub alert backend: it records the query
// each request carried (so the bounds can be asserted on the wire, not just on
// the request struct) and serves a fixed page per feed.
type securityAlertServer struct {
	mu       sync.Mutex
	queries  []url.Values
	codeScan string
	depabot  string
	status   int
	body     string
}

func (s *securityAlertServer) start(t *testing.T) *GitHubProvider {
	t.Helper()
	mux := http.NewServeMux()
	record := func(w http.ResponseWriter, r *http.Request, payload string) {
		s.mu.Lock()
		s.queries = append(s.queries, r.URL.Query())
		status, body := s.status, s.body
		s.mu.Unlock()
		if status != 0 {
			http.Error(w, body, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}
	mux.HandleFunc("/repos/acme/app/code-scanning/alerts", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, s.codeScan)
	})
	mux.HandleFunc("/repos/acme/app/dependabot/alerts", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, s.depabot)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	provider := NewGitHubProvider("token")
	provider.BaseURL = server.URL
	return provider
}

func (s *securityAlertServer) lastQuery(t *testing.T) url.Values {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queries) == 0 {
		t.Fatal("no request reached the server")
	}
	return s.queries[len(s.queries)-1]
}

func alertRepo() RepositoryRef {
	return RepositoryRef{Provider: ProviderGitHub, Owner: "acme", Name: "app"}
}

const codeScanningPage = `[
  {"number": 11, "html_url": "https://example/alerts/11", "state": "open",
   "created_at": "2026-09-01T00:00:00Z", "updated_at": "2026-09-02T00:00:00Z",
   "rule": {"id": "go/sql-injection", "name": "SQL injection", "severity": "error",
            "security_severity_level": "high", "description": "Untrusted input reaches a query."},
   "tool": {"name": "CodeQL", "version": "2.18.0"},
   "most_recent_instance": {"ref": "refs/heads/main", "commit_sha": "abc123",
     "message": {"text": "This query depends on a user-provided value."},
     "location": {"path": "internal/db/query.go", "start_line": 42, "end_line": 44}}},
  {"number": 12, "html_url": "https://example/alerts/12", "state": "open",
   "rule": {"id": "go/clear-text-logging", "name": "Clear-text logging", "severity": "warning",
            "security_severity_level": "critical", "description": "Sensitive data is logged."},
   "tool": {"name": "CodeQL", "version": "2.18.0"},
   "most_recent_instance": {"ref": "refs/heads/main",
     "message": {"text": "Sensitive value logged."},
     "location": {"path": "internal/log/log.go", "start_line": 7, "end_line": 7}}}
]`

const dependabotPage = `[
  {"number": 11, "html_url": "https://example/dependabot/11", "state": "open",
   "created_at": "2026-09-01T00:00:00Z",
   "dependency": {"package": {"ecosystem": "npm", "name": "tar"},
                  "manifest_path": "portal/package-lock.json",
                  "scope": "development", "relationship": "transitive"},
   "security_advisory": {"ghsa_id": "GHSA-2v37-7h3g-55p8", "cve_id": null,
     "summary": "Arbitrary file write in tar", "severity": "high",
     "identifiers": [{"type": "GHSA", "value": "GHSA-2v37-7h3g-55p8"},
                     {"type": "CVE", "value": "CVE-2026-1234"}]},
   "security_vulnerability": {"severity": "high",
     "vulnerable_version_range": "< 6.2.1",
     "first_patched_version": {"identifier": "6.2.1"}}}
]`

// The default read is bounded and scoped by construction: open alerts only, on
// the requested ref, with per_page at the provider ceiling. An intake that
// silently defaulted to every alert ever reported would be neither.
func TestListSecurityAlertsSendsBoundedScopedQuery(t *testing.T) {
	server := &securityAlertServer{codeScan: codeScanningPage}
	provider := server.start(t)

	if _, err := provider.ListSecurityAlerts(context.Background(), ListSecurityAlertsRequest{
		Repository: alertRepo(),
		Source:     SecurityAlertCodeScanning,
		Ref:        "refs/heads/main",
		Severity:   []string{"critical", "high"},
		Tool:       "CodeQL",
	}); err != nil {
		t.Fatalf("ListSecurityAlerts: %v", err)
	}
	query := server.lastQuery(t)
	for field, want := range map[string]string{
		"state":     "open",
		"ref":       "refs/heads/main",
		"severity":  "critical,high",
		"tool_name": "CodeQL",
	} {
		if got := query.Get(field); got != want {
			t.Errorf("query %s = %q, want %q", field, got, want)
		}
	}
	if query.Get("per_page") == "" {
		t.Error("query carries no per_page; the walk must be paged explicitly")
	}
}

func TestListSecurityAlertsMapsCodeScanningAlerts(t *testing.T) {
	server := &securityAlertServer{codeScan: codeScanningPage}
	provider := server.start(t)

	alerts, err := provider.ListSecurityAlerts(context.Background(), ListSecurityAlertsRequest{
		Repository: alertRepo(),
		Source:     SecurityAlertCodeScanning,
	})
	if err != nil {
		t.Fatalf("ListSecurityAlerts: %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %d, want 2", len(alerts))
	}
	// Ordering is severity-first, so the critical alert leads regardless of the
	// order the provider paged them in.
	if alerts[0].Number != 12 || alerts[0].Severity != "critical" {
		t.Fatalf("first alert = %+v, want the critical one first", alerts[0])
	}
	// security_severity_level wins over the tool's own error/warning level:
	// "error" is a lint level, not a security ranking.
	if alerts[1].Severity != "high" {
		t.Errorf("severity = %q, want the CVSS-derived high over the tool's error level", alerts[1].Severity)
	}
	detail := alerts[1].CodeScanning
	if detail == nil || detail.RuleID != "go/sql-injection" || detail.Path != "internal/db/query.go" ||
		detail.StartLine != 42 || detail.ToolName != "CodeQL" || detail.Ref != "refs/heads/main" {
		t.Fatalf("code-scanning detail = %+v", detail)
	}
	for _, alert := range alerts {
		if alert.Integrity != apiintegrity.Unapproved {
			t.Fatalf("alert %d integrity = %q, want unapproved — rule text and paths are repository content",
				alert.Number, alert.Integrity)
		}
	}
}

func TestListSecurityAlertsMapsDependabotAlerts(t *testing.T) {
	server := &securityAlertServer{depabot: dependabotPage}
	provider := server.start(t)

	alerts, err := provider.ListSecurityAlerts(context.Background(), ListSecurityAlertsRequest{
		Repository: alertRepo(),
		Source:     SecurityAlertDependabot,
		Ecosystem:  []string{"npm"},
		Scope:      "development",
	})
	if err != nil {
		t.Fatalf("ListSecurityAlerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want 1", len(alerts))
	}
	detail := alerts[0].Dependabot
	if detail == nil {
		t.Fatal("dependabot detail missing")
	}
	// The CVE is recovered from the identifier list: GitHub reports cve_id as
	// null for advisories that carry the CVE only as an identifier entry, and
	// a nominator correlating on CVE would otherwise see nothing.
	if detail.CVEID != "CVE-2026-1234" {
		t.Errorf("cveId = %q, want the identifier-list CVE when cve_id is null", detail.CVEID)
	}
	if detail.GHSAID != "GHSA-2v37-7h3g-55p8" || detail.Ecosystem != "npm" || detail.Package != "tar" ||
		detail.ManifestPath != "portal/package-lock.json" || detail.Scope != "development" ||
		detail.Relationship != "transitive" || detail.FirstPatchedVersion != "6.2.1" {
		t.Fatalf("dependabot detail = %+v", detail)
	}
	query := server.lastQuery(t)
	if query.Get("ecosystem") != "npm" || query.Get("scope") != "development" {
		t.Errorf("query = %v, want the ecosystem and scope bounds on the wire", query)
	}
}

// The dedupe identity is what stops a scheduled intake from filing one issue
// per scan or per data-flow location (#2984, #4230).
func TestSecurityAlertDedupeKeyIdentifiesADefectNotAnOccurrence(t *testing.T) {
	base := SecurityAlert{
		Source: SecurityAlertCodeScanning,
		Number: 11,
		CodeScanning: &CodeScanningDetail{
			ToolName: "CodeQL", RuleID: "go/sql-injection", Path: "internal/db/query.go", StartLine: 42,
		},
	}
	renumbered := base
	renumbered.Number = 97
	relocated := base
	relocated.CodeScanning = &CodeScanningDetail{
		ToolName: "CodeQL", RuleID: "go/sql-injection", Path: "internal/db/query.go", StartLine: 512,
	}
	elsewhere := base
	elsewhere.CodeScanning = &CodeScanningDetail{
		ToolName: "CodeQL", RuleID: "go/sql-injection", Path: "internal/db/other.go",
	}

	key := SecurityAlertDedupeKey(base)
	if key == "" {
		t.Fatal("dedupe key is empty")
	}
	// A re-scan can renumber alerts; keying on the number would file a fresh
	// issue for a defect already tracked.
	if got := SecurityAlertDedupeKey(renumbered); got != key {
		t.Errorf("renumbered key = %q, want %q — the alert number is not identity", got, key)
	}
	// One rule firing at several data-flow lines in one file is one defect.
	if got := SecurityAlertDedupeKey(relocated); got != key {
		t.Errorf("relocated key = %q, want %q — a line number is not identity", got, key)
	}
	// A different file is a different defect.
	if got := SecurityAlertDedupeKey(elsewhere); got == key {
		t.Errorf("key for a different path = %q, want it to differ from %q", got, key)
	}

	advisory := SecurityAlert{
		Source: SecurityAlertDependabot,
		Dependabot: &DependabotDetail{
			GHSAID: "GHSA-2v37-7h3g-55p8", Ecosystem: "npm", Package: "tar",
			ManifestPath: "portal/package-lock.json",
		},
	}
	otherManifest := advisory
	otherManifest.Dependabot = &DependabotDetail{
		GHSAID: "GHSA-2v37-7h3g-55p8", Ecosystem: "npm", Package: "tar",
		ManifestPath: "examples/package-lock.json",
	}
	if SecurityAlertDedupeKey(advisory) == SecurityAlertDedupeKey(otherManifest) {
		t.Error("one advisory against two manifests must be two nominations")
	}
}

// A bounded read stops at the cap rather than draining the feed.
func TestListSecurityAlertsHonoursTheLimit(t *testing.T) {
	server := &securityAlertServer{codeScan: codeScanningPage}
	provider := server.start(t)

	alerts, err := provider.ListSecurityAlerts(context.Background(), ListSecurityAlertsRequest{
		Repository: alertRepo(),
		Source:     SecurityAlertCodeScanning,
		Limit:      1,
	})
	if err != nil {
		t.Fatalf("ListSecurityAlerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want 1", len(alerts))
	}
}

// A provider error is reported, never flattened into an empty list — an empty
// list is indistinguishable from "this repository has no alerts", which is
// exactly the wrong conclusion for a missing permission.
func TestListSecurityAlertsReportsProviderErrors(t *testing.T) {
	server := &securityAlertServer{status: http.StatusForbidden, body: `{"message":"Resource not accessible by personal access token"}`}
	provider := server.start(t)

	alerts, err := provider.ListSecurityAlerts(context.Background(), ListSecurityAlertsRequest{
		Repository: alertRepo(),
		Source:     SecurityAlertDependabot,
	})
	if err == nil {
		t.Fatalf("err = nil, alerts = %v; want the permission failure surfaced", alerts)
	}
	if !strings.Contains(err.Error(), "dependabot") {
		t.Errorf("error = %q, want it to name the feed", err)
	}
}

func TestValidateListSecurityAlertsRequestRejectsCrossFeedFilters(t *testing.T) {
	tests := map[string]ListSecurityAlertsRequest{
		"missing source":             {Repository: alertRepo()},
		"unknown source":             {Repository: alertRepo(), Source: "sarif"},
		"ref on dependabot":          {Repository: alertRepo(), Source: SecurityAlertDependabot, Ref: "refs/heads/main"},
		"ecosystem on code scanning": {Repository: alertRepo(), Source: SecurityAlertCodeScanning, Ecosystem: []string{"npm"}},
		"bad scope":                  {Repository: alertRepo(), Source: SecurityAlertDependabot, Scope: "everything"},
		"limit over ceiling":         {Repository: alertRepo(), Source: SecurityAlertDependabot, Limit: MaxSecurityAlertLimit + 1},
	}
	for name, req := range tests {
		if err := ValidateListSecurityAlertsRequest(req); err == nil {
			t.Errorf("%s: err = nil, want a rejection", name)
		}
	}
	if err := ValidateListSecurityAlertsRequest(ListSecurityAlertsRequest{
		Repository: alertRepo(), Source: SecurityAlertCodeScanning, Ref: "refs/heads/main",
	}); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
}

// GitHub declares both feeds; the capability set is what lets a workflow's
// provider requirements be checked before any credential is resolved.
func TestGitHubDeclaresBothSecurityAlertCapabilities(t *testing.T) {
	set := (&GitHubProvider{}).Capabilities()
	for _, cap := range []Capability{CapSecurityAlertsCodeScanning, CapSecurityAlertsDependabot} {
		if !set.Has(cap) {
			t.Errorf("GitHub does not declare %s", cap)
		}
	}
	for _, kind := range []ProviderKind{ProviderADO, ProviderGitea} {
		set, ok := CapabilitiesFor(kind)
		if !ok {
			t.Fatalf("no capability set for %s", kind)
		}
		// Neither backend implements SecurityAlertLister, so neither may
		// declare the surface: a declaration nothing implements is exactly the
		// fail-open stub the capability model exists to prevent.
		if set.Has(CapSecurityAlertsCodeScanning) || set.Has(CapSecurityAlertsDependabot) {
			t.Errorf("%s declares a security-alert capability it does not implement", kind)
		}
	}
	var _ SecurityAlertLister = (*GitHubProvider)(nil)
}

// The artifact a stage emits is JSON, so the wire shape is part of the
// contract: an alert must round-trip without losing its dedupe identity.
func TestSecurityAlertRoundTripsThroughJSON(t *testing.T) {
	server := &securityAlertServer{depabot: dependabotPage}
	provider := server.start(t)
	alerts, err := provider.ListSecurityAlerts(context.Background(), ListSecurityAlertsRequest{
		Repository: alertRepo(), Source: SecurityAlertDependabot,
	})
	if err != nil {
		t.Fatalf("ListSecurityAlerts: %v", err)
	}
	data, err := json.Marshal(alerts[0])
	if err != nil {
		t.Fatal(err)
	}
	var decoded SecurityAlert
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.DedupeKey != alerts[0].DedupeKey || decoded.Integrity != apiintegrity.Unapproved {
		t.Fatalf("round trip = %+v, want the dedupe key and integrity preserved", decoded)
	}
}
