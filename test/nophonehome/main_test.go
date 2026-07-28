package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanRejectsBuiltInEgressDestinations(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{
			name: "direct HTTP destination",
			source: `package sample
import "net/http"
func report() { _, _ = http.NewRequest(http.MethodPost, "https://maintainer.example.invalid/usage", nil) }
`,
			want: "hardcoded network destination",
		},
		{
			name: "constant destination",
			source: `package sample
import "google.golang.org/grpc"
const collector = "maintainer.example.invalid:4317"
func report() { _, _ = grpc.NewClient(collector) }
`,
			want: "hardcoded network destination",
		},
		{
			name: "local destination",
			source: `package sample
import "net/http"
func report() {
	endpoint := "https://maintainer.example.invalid/usage"
	_, _ = http.Post(endpoint, "application/json", nil)
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "conditional maintainer-owned neutral path fallback",
			source: `package sample
import "net/http"
type Config struct { ReportEndpoint string }
func report(cfg Config) {
	endpoint := cfg.ReportEndpoint
	if endpoint == "" {
		endpoint = "https://agent-clubhouse.github.io/goobers/events"
	}
	_, _ = http.Post(endpoint, "application/json", nil)
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "helper-wrapped maintainer-owned neutral path fallback",
			source: `package sample
import "net/http"
type Config struct { ReportEndpoint string }
func firstNonEmpty(values ...string) string { return "" }
func report(cfg Config) {
	endpoint := firstNonEmpty(cfg.ReportEndpoint, "https://agent-clubhouse.github.io/goobers/events")
	_, _ = http.Post(endpoint, "application/json", nil)
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "maintainer-owned neutral path in config field",
			source: `package sample
import "net/http"
type Config struct { ReportURL string }
func report() {
	cfg := Config{ReportURL: "https://agent-clubhouse.github.io/goobers/events"}
	_, _ = http.Post(cfg.ReportURL, "application/json", nil)
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "HTTP client destination",
			source: `package sample
import "net/http"
func report() {
	client := &http.Client{}
	_, _ = client.Post("https://maintainer.example.invalid/usage", "application/json", nil)
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "default HTTP client destination",
			source: `package sample
import "net/http"
func report() {
	_, _ = http.DefaultClient.Post("https://maintainer.example.invalid/usage", "application/json", nil)
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "process argument destination",
			source: `package sample
import "os/exec"
func report() { _ = exec.Command("curl", "https://maintainer.example.invalid/usage").Run() }
`,
			want: "hardcoded network destination",
		},
		{
			name: "net dialer destination",
			source: `package sample
import (
	"context"
	"net"
)
func report(ctx context.Context) {
	_, _ = (&net.Dialer{}).DialContext(ctx, "tcp", "maintainer.example.invalid:443")
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "net dialer struct field",
			source: `package sample
import (
	"context"
	"net"
)
type reporter struct { dialer *net.Dialer }
func (r *reporter) report(ctx context.Context) {
	_, _ = r.dialer.DialContext(ctx, "tcp", "maintainer.example.invalid:443")
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "unlisted process argument destination",
			source: `package sample
import "os/exec"
func report() { _ = exec.Command("git", "push", "https://maintainer.example.invalid/usage").Run() }
`,
			want: "hardcoded network destination",
		},
		{
			name: "mixed static and dynamic destination",
			source: `package sample
import "net/http"
func report(runID string) { _, _ = http.Get("https://maintainer.example.invalid/usage/" + runID) }
`,
			want: "hardcoded network destination",
		},
		{
			name: "formatted static origin",
			source: `package sample
import (
	"fmt"
	"net/http"
)
func report() { _, _ = http.Get(fmt.Sprintf("https://%s/usage", "maintainer.example.invalid")) }
`,
			want: "hardcoded network destination",
		},
		{
			name: "formatted origin argument",
			source: `package sample
import (
	"fmt"
	"net/http"
)
func report(runID string) {
	_, _ = http.Get(fmt.Sprintf("%s/usage/%s", "https://maintainer.example.invalid", runID))
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "HTTP client struct field",
			source: `package sample
import "net/http"
type reporter struct { client *http.Client }
func (r *reporter) report() {
	_, _ = r.client.Post("https://maintainer.example.invalid/usage", "application/json", nil)
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "embedded HTTP client",
			source: `package sample
import "net/http"
type reporter struct { *http.Client }
func (r *reporter) report() {
	_, _ = r.Get("https://maintainer.example.invalid/usage")
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "transitively embedded HTTP client",
			source: `package sample
import "net/http"
type inner struct { *http.Client }
type reporter struct { inner }
func (r *reporter) report() {
	_, _ = r.Get("https://maintainer.example.invalid/usage")
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "HTTP client request",
			source: `package sample
import (
	"net/http"
	"net/url"
)
func report(client *http.Client, runID string) {
	request := &http.Request{}
	request.URL, _ = url.Parse("https://maintainer.example.invalid/usage/" + runID)
	_, _ = client.Do(request)
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "HTTP client URL literal request",
			source: `package sample
import (
	"net/http"
	"net/url"
)
func report(client *http.Client) {
	request := &http.Request{URL: &url.URL{
		Scheme: "https",
		Host: "maintainer.example.invalid",
		Path: "/usage",
	}}
	_, _ = client.Do(request)
}
`,
			want: "hardcoded network destination",
		},
		{
			name: "default telemetry endpoint",
			source: `package sample
func configure() {
	var cfg struct{ OTLPEndpoint string }
	cfg.OTLPEndpoint = "maintainer.example.invalid:4317"
}
`,
			want: "default telemetry/reporting endpoint",
		},
		{
			name: "locally aliased default telemetry endpoint",
			source: `package sample
func configure() {
	var cfg struct{ OTLPEndpoint string }
	endpoint := "maintainer.example.invalid:4317"
	cfg.OTLPEndpoint = endpoint
}
`,
			want: "default telemetry/reporting endpoint",
		},
		{
			name: "implicit exporter",
			source: `package sample
import "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
func report() { _, _ = otlptracegrpc.New(nil) }
`,
			want: "implicit network destination",
		},
		{
			name: "hardcoded crash reporting DSN",
			source: `package sample
import "github.com/getsentry/sentry-go"
func report() {
	_ = sentry.Init(sentry.ClientOptions{Dsn: "https://key@maintainer.ingest.example/1"})
}
`,
			want: "hardcoded reporting SDK destination",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeSource(t, test.source)
			findings, err := scan(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) == 0 || !strings.Contains(findings[0].message, test.want) {
				t.Fatalf("scan() findings = %#v, want %q", findings, test.want)
			}
		})
	}
}

func TestScanRejectsBuiltInScriptEgressDestination(t *testing.T) {
	root := writeSourceAt(t, "portal/src/report.ts", `
export async function report(): Promise<void> {
  await fetch("https://maintainer.example.invalid/usage", { method: "POST" });
}
`)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 || !strings.Contains(findings[0].message, "hardcoded network destination") {
		t.Fatalf("scan() findings = %#v, want hardcoded network destination", findings)
	}
}

func TestScanRejectsBuiltInCommandEgressDestinations(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		source string
	}{
		{
			name:   "shell",
			path:   "scripts/report.sh",
			source: "#!/bin/sh\ncurl https://maintainer.example.invalid/usage\n",
		},
		{
			name:   "PowerShell",
			path:   "scripts/report.ps1",
			source: "Invoke-RestMethod -Uri https://maintainer.example.invalid/usage\n",
		},
		{
			name:   "Makefile",
			path:   "Makefile",
			source: "report:\n\tcurl https://maintainer.example.invalid/usage\n",
		},
		{
			name: "workflow",
			path: ".github/workflows/report.yml",
			source: `jobs:
  report:
    steps:
      - run: curl https://maintainer.example.invalid/usage
`,
		},
		{
			name: "maintainer-owned neutral path",
			path: ".github/workflows/report.yml",
			source: `jobs:
  report:
    steps:
      - run: curl https://agent-clubhouse.github.io/goobers/events
`,
		},
		{
			name:   "shell binding",
			path:   "scripts/report.sh",
			source: "REPORT_URL=https://maintainer.example.invalid/usage\ncurl \"$REPORT_URL\"\n",
		},
		{
			name:   "PowerShell binding",
			path:   "scripts/report.ps1",
			source: "$ReportURL = \"https://maintainer.example.invalid/usage\"\nInvoke-RestMethod -Uri $ReportURL\n",
		},
		{
			name:   "Make binding",
			path:   "Makefile",
			source: "REPORT_URL := https://maintainer.example.invalid/usage\nreport:\n\tcurl \"$(REPORT_URL)\"\n",
		},
		{
			name: "workflow environment binding",
			path: ".github/workflows/report.yml",
			source: `jobs:
  report:
    steps:
      - env:
          REPORT_URL: https://maintainer.example.invalid/usage
        run: curl "$REPORT_URL"
`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeSourceAt(t, test.path, test.source)
			findings, err := scan(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) == 0 ||
				!strings.Contains(findings[0].message, "hardcoded telemetry/reporting destination") {
				t.Fatalf("scan() findings = %#v, want hardcoded telemetry/reporting destination", findings)
			}
		})
	}
}

func TestScanAllowsCommandDependencyDownload(t *testing.T) {
	root := writeSourceAt(t, ".github/workflows/ci.yml", `
jobs:
  lint:
    steps:
      - run: |
          curl -sSfL "https://raw.githubusercontent.com/golangci/golangci-lint/install.sh" \
            | sh
`)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("scan() findings = %v, want none", findings)
	}
}

func TestScanAllowsEscapedMakeVariables(t *testing.T) {
	root := writeSourceAt(t, "Makefile", `fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "$$unformatted"; exit 1; \
	fi
`)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("scan() findings = %v, want none", findings)
	}
}

func TestScanRejectsAliasedScriptEgressDestination(t *testing.T) {
	root := writeSourceAt(t, "portal/src/report.ts", `
const endpoint = "https://maintainer.example.invalid/usage";
export async function report(): Promise<void> {
  await fetch(endpoint, { method: "POST" });
}
`)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 || !strings.Contains(findings[0].message, "hardcoded network destination") {
		t.Fatalf("scan() findings = %#v, want hardcoded network destination", findings)
	}
}

func TestScanRejectsInterpolatedScriptEgressDestination(t *testing.T) {
	root := writeSourceAt(t, "portal/src/report.ts", `
export async function report(runID: string): Promise<void> {
  await fetch(`+"`https://maintainer.example.invalid/usage/${runID}`"+`, { method: "POST" });
}
`)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 || !strings.Contains(findings[0].message, "hardcoded network destination") {
		t.Fatalf("scan() findings = %#v, want hardcoded network destination", findings)
	}
}

func TestScanRejectsStaticallyInterpolatedScriptOrigin(t *testing.T) {
	root := writeSourceAt(t, "portal/src/report.ts", `
const host = "maintainer.example.invalid";
export async function report(runID: string): Promise<void> {
  await fetch(`+"`https://${host}/usage/${runID}`"+`, { method: "POST" });
}
`)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 || !strings.Contains(findings[0].message, "hardcoded network destination") {
		t.Fatalf("scan() findings = %#v, want hardcoded network destination", findings)
	}
}

func TestScanRejectsScriptEgressAfterRegexLiteral(t *testing.T) {
	root := writeSourceAt(t, "portal/src/report.ts", `
const quote = /['"]/;
export async function report(): Promise<void> {
  await fetch("https://maintainer.example.invalid/usage", { method: "POST" });
}
`)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 || !strings.Contains(findings[0].message, "hardcoded network destination") {
		t.Fatalf("scan() findings = %#v, want hardcoded network destination", findings)
	}
}

func TestScanTracksScriptBindingScopesAndAssignments(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{
			name: "outer binding restored after shadow",
			source: `
const target = "https://maintainer.example.invalid/usage";
function report(userTarget: string): void {
  const target = userTarget;
}
void fetch(target);
`,
		},
		{
			name: "hardcoded reassignment",
			source: `
let target = userTarget;
target = "https://maintainer.example.invalid/usage";
void fetch(target);
`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeSourceAt(t, "portal/src/report.ts", test.source)
			findings, err := scan(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) == 0 || !strings.Contains(findings[0].message, "hardcoded network destination") {
				t.Fatalf("scan() findings = %#v, want hardcoded network destination", findings)
			}
		})
	}
}

func TestScanAllowsUserSuppliedDestinationAndIgnoresTests(t *testing.T) {
	root := writeSource(t, `package sample
import (
	"fmt"
	"net/http"
	"os/exec"
)
func report(endpoint, runID string) {
	_, _ = http.NewRequest(http.MethodPost, endpoint + "/usage/" + runID, nil)
	_, _ = http.Get(endpoint + "?redirect=https://docs.example.invalid")
	_, _ = http.Get(fmt.Sprintf("%s/usage/%s?docs=%s", endpoint, runID, "https://docs.example.invalid"))
	_ = exec.Command("curl", endpoint).Run()
	_ = exec.Command("git", "clone", fmt.Sprintf("https://github.com/%s/%s.git", "owner", runID)).Run()
	client := &http.Client{}
	request := makeRequest(endpoint, "https://docs.example.invalid")
	_, _ = client.Do(request)
}
`)
	if err := os.WriteFile(filepath.Join(root, "hardcoded_test.go"), []byte(`package sample
import "net/http"
func fixture() { _, _ = http.Get("https://fixture.example.invalid") }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hardcoded.test.ts"), []byte(`
void fetch("https://fixture.example.invalid");
`), 0o600); err != nil {
		t.Fatal(err)
	}

	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("scan() findings = %v, want none", findings)
	}
}

func TestScanRejectsUserConfiguredReportingSDKDestination(t *testing.T) {
	root := writeSource(t, `package sample
import "github.com/getsentry/sentry-go"
type Config struct { SentryDSN string }
func report(cfg Config) {
	_ = sentry.Init(sentry.ClientOptions{Dsn: cfg.SentryDSN})
}
`)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 ||
		!strings.Contains(findings[0].message, "non-OTLP reporting SDK initialization") {
		t.Fatalf("scan() findings = %#v, want non-OTLP reporting SDK initialization", findings)
	}
}

func TestScanRejectsEnvironmentConfiguredReportingSDKDestination(t *testing.T) {
	root := writeSource(t, `package sample
import (
	"os"
	"github.com/getsentry/sentry-go"
)
func report() {
	_ = sentry.Init(sentry.ClientOptions{Dsn: os.Getenv("SENTRY_DSN")})
}
`)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 ||
		!strings.Contains(findings[0].message, "non-OTLP reporting SDK initialization") {
		t.Fatalf("scan() findings = %#v, want non-OTLP reporting SDK initialization", findings)
	}
}

func TestScanAllowsUserSuppliedScriptDestination(t *testing.T) {
	root := writeSourceAt(t, "portal/src/report.ts", `
const requestUrl = "https://docs.example.invalid";
export async function report(endpoint: string): Promise<void> {
  const runID = "run";
  const requestUrl = `+"`${endpoint}/usage/${runID}`"+`;
  await fetch(requestUrl, { method: "POST" });
}
`)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("scan() findings = %v, want none", findings)
	}
}

func TestScanAllowsSingleConfiguredImplicitExporter(t *testing.T) {
	root := writeSourceAt(t, "internal/telemetry/client.go", configuredExporterSource(false))
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("scan() findings = %v, want none", findings)
	}
}

func TestScanRejectsExtraImplicitExporterInApprovedFunction(t *testing.T) {
	root := writeSourceAt(t, "internal/telemetry/client.go", configuredExporterSource(true))
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 || !strings.Contains(findings[0].message, "implicit network destination") {
		t.Fatalf("scan() findings = %#v, want implicit network destination", findings)
	}
}

func TestScanRejectsUnconfiguredExporterInApprovedFunction(t *testing.T) {
	source := strings.Replace(
		configuredExporterSource(false),
		"\topts = append(opts, otlptracegrpc.WithEndpoint(endpoint))\n",
		"",
		1,
	)
	root := writeSourceAt(t, "internal/telemetry/client.go", source)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 || !strings.Contains(findings[0].message, "implicit network destination") {
		t.Fatalf("scan() findings = %#v, want implicit network destination", findings)
	}
}

func TestScanRejectsConditionallyConfiguredExporterInApprovedFunction(t *testing.T) {
	source := strings.Replace(
		configuredExporterSource(false),
		"\topts = append(opts, otlptracegrpc.WithEndpoint(endpoint))\n",
		"\tif false {\n\t\topts = append(opts, otlptracegrpc.WithEndpoint(endpoint))\n\t}\n",
		1,
	)
	root := writeSourceAt(t, "internal/telemetry/client.go", source)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 || !strings.Contains(findings[0].message, "implicit network destination") {
		t.Fatalf("scan() findings = %#v, want implicit network destination", findings)
	}
}

func TestScanRejectsMutatedApprovedConfigSource(t *testing.T) {
	source := strings.Replace(
		configuredExporterSource(false),
		"func spanExporters(ctx any, cfg struct{ OTLPEndpoint string }) {\n",
		"type Config struct{ OTLPEndpoint string }\n"+
			"func spanExporters(ctx any, cfg Config) {\n"+
			"\tcfg = Config{OTLPEndpoint: \"maintainer.example.invalid:4317\"}\n",
		1,
	)
	root := writeSourceAt(t, "internal/telemetry/client.go", source)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var defaultEndpoint, implicitExporter bool
	for _, finding := range findings {
		defaultEndpoint = defaultEndpoint ||
			strings.Contains(finding.message, "default telemetry/reporting endpoint")
		implicitExporter = implicitExporter ||
			strings.Contains(finding.message, "implicit network destination")
	}
	if !defaultEndpoint || !implicitExporter {
		t.Fatalf("scan() findings = %#v, want default endpoint and implicit exporter", findings)
	}
}

func TestScanRejectsConditionalEndpointFallback(t *testing.T) {
	source := strings.Replace(
		configuredExporterSource(false),
		"\tendpoint := strings.TrimSpace(cfg.OTLPEndpoint)\n",
		"\tendpoint := strings.TrimSpace(cfg.OTLPEndpoint)\n"+
			"\tif endpoint == \"\" {\n\t\tendpoint = \"maintainer.example.invalid:4317\"\n\t}\n",
		1,
	)
	root := writeSourceAt(t, "internal/telemetry/client.go", source)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 || !strings.Contains(findings[0].message, "hardcoded network destination") {
		t.Fatalf("scan() findings = %#v, want hardcoded network destination", findings)
	}
}

func TestScanRejectsHelperEndpointFallback(t *testing.T) {
	source := strings.Replace(
		configuredExporterSource(false),
		"\tendpoint := strings.TrimSpace(cfg.OTLPEndpoint)\n",
		"\tendpoint := firstNonEmpty(cfg.OTLPEndpoint, \"maintainer.example.invalid:4317\")\n",
		1,
	)
	root := writeSourceAt(t, "internal/telemetry/client.go", source)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 || !strings.Contains(findings[0].message, "implicit network destination") {
		t.Fatalf("scan() findings = %#v, want implicit network destination", findings)
	}
}

func TestScanAllowsConditionalMutationAfterEndpointCapture(t *testing.T) {
	source := strings.Replace(
		configuredExporterSource(false),
		"\tendpoint := strings.TrimSpace(cfg.OTLPEndpoint)\n",
		"\tbase := strings.TrimSpace(cfg.OTLPEndpoint)\n"+
			"\tendpoint := base\n"+
			"\tif base == \"\" {\n\t\tbase = \"maintainer.example.invalid:4317\"\n\t}\n",
		1,
	)
	root := writeSourceAt(t, "internal/telemetry/client.go", source)
	findings, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("scan() findings = %v, want none", findings)
	}
}

func TestRunReportsGuardFailure(t *testing.T) {
	root := writeSource(t, `package sample
import "net/http"
func report() { _, _ = http.Get("https://maintainer.example.invalid/usage") }
`)
	var stdout, stderr bytes.Buffer
	if code := run([]string{root}, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "SEC-048") || stdout.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", &stdout, &stderr)
	}
}

func writeSource(t *testing.T, source string) string {
	t.Helper()
	return writeSourceAt(t, "sample.go", source)
}

func writeSourceAt(t *testing.T, path, source string) string {
	t.Helper()
	root := t.TempDir()
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func configuredExporterSource(extra bool) string {
	source := `package telemetry
import (
	"strings"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
)
func spanExporters(ctx any, cfg struct{ OTLPEndpoint string }) {
	endpoint := strings.TrimSpace(cfg.OTLPEndpoint)
	opts := []otlptracegrpc.Option{}
	opts = append(opts, otlptracegrpc.WithEndpoint(endpoint))
	_, _ = otlptracegrpc.New(ctx, opts...)
`
	if extra {
		source += "\t_, _ = otlptracegrpc.New(ctx)\n"
	}
	return source + "}\n"
}
