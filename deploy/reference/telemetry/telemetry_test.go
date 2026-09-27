package telemetry_test

import (
	"os"
	"strings"
	"testing"
)

func TestTelemetryOverlayUsesSecretReferenceAndPersistentInstanceRoot(t *testing.T) {
	patch := read(t, "api-telemetry.yaml")
	for _, want := range []string{
		"name: APPLICATIONINSIGHTS_CONNECTION_STRING",
		"secretKeyRef:",
		"name: goobers-application-insights",
		"key: connection-string",
		"--connection-string-env",
		"--profile",
		"standard",
		"mountPath: /var/lib/goobers",
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("api-telemetry.yaml missing %q", want)
		}
	}
	if strings.Contains(patch, "InstrumentationKey=") {
		t.Fatal("deployment patch must not contain a connection-string value")
	}

	secret := read(t, "application-insights-secret.example.yaml")
	for _, want := range []string{"kind: Secret", "name: goobers-application-insights", "connection-string:"} {
		if !strings.Contains(secret, want) {
			t.Errorf("Secret example missing %q", want)
		}
	}

	docs := read(t, "README.md")
	for _, want := range []string{"goobers telemetry test /var/lib/goobers", "does not change the running daemon", "journal PVC"} {
		if !strings.Contains(docs, want) {
			t.Errorf("README missing %q", want)
		}
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
