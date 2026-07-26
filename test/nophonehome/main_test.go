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
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
