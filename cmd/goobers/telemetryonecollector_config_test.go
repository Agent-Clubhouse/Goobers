package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOneCollectorConfigurePreservesYAMLWithoutPersistingEnvironment(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "instance.yaml")
	raw := "# synthetic retained comment\ntelemetry:\n  enabled: true # retain public selection\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_OTLP_ENDPOINT", "https://synthetic-public.example.invalid:4317")
	var stdout, stderr bytes.Buffer
	if code := runTelemetryConfigure([]string{"--onecollector-profile-file", writeSyntheticCollectorProfile(t), "--json", root}, &stdout, &stderr); code != 0 {
		t.Fatal(code, stderr.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# synthetic retained comment", "enabled: true # retain public selection", "profile_file:"} {
		if !bytes.Contains(got, []byte(want)) {
			t.Fatal("configuration content lost", want)
		}
	}
	if strings.Contains(string(got), "synthetic-public") || strings.Contains(string(got), "otlp:") {
		t.Fatal("unrelated environment override was persisted")
	}
}

func TestOneCollectorConfigureRefusesExistingInlineProfile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "instance.yaml")
	raw := "telemetry:\n  enabled: false\n  onecollector:\n    synthetic_inline: true\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runTelemetryConfigure([]string{"--onecollector-profile-file", writeSyntheticCollectorProfile(t), root}, &stdout, &stderr); code == 0 {
		t.Fatal("existing unsupported profile replaced")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != raw {
		t.Fatal("refusal changed user configuration", err)
	}
}
