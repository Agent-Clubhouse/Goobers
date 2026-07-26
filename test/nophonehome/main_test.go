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

func TestScanAllowsUserSuppliedDestinationAndIgnoresTests(t *testing.T) {
	root := writeSource(t, `package sample
import "net/http"
func report(endpoint string) { _, _ = http.NewRequest(http.MethodPost, endpoint, nil) }
`)
	if err := os.WriteFile(filepath.Join(root, "hardcoded_test.go"), []byte(`package sample
import "net/http"
func fixture() { _, _ = http.Get("https://fixture.example.invalid") }
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
