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
