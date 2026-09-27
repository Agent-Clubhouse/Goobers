package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"

	"sigs.k8s.io/yaml"
)

func TestGaggleBundleCLIExportAndImport(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}

	var first, second, stderr bytes.Buffer
	if code := runGaggleExport([]string{"example", root}, &first, &stderr); code != 0 {
		t.Fatalf("first export code = %d, stderr = %s", code, stderr.String())
	}
	stderr.Reset()
	if code := runGaggleExport([]string{"example", root}, &second, &stderr); code != 0 {
		t.Fatalf("second export code = %d, stderr = %s", code, stderr.String())
	}
	var firstBundle, secondBundle apiv1.GaggleBundle
	if err := json.Unmarshal(first.Bytes(), &firstBundle); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Bytes(), &secondBundle); err != nil {
		t.Fatal(err)
	}
	if firstBundle.Digest != secondBundle.Digest {
		t.Fatalf("unchanged CLI export digest moved: %s != %s", firstBundle.Digest, secondBundle.Digest)
	}
	unknown := bytes.Replace(first.Bytes(), []byte(`"definition": {`), []byte(`"definition": {"unexpected": true,`), 1)
	unknownPath := filepath.Join(t.TempDir(), "unknown.bundle.json")
	if err := os.WriteFile(unknownPath, unknown, 0o644); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := runGaggleImport([]string{"--name", "unknown", unknownPath, root}, &bytes.Buffer{}, &stderr); code != 1 {
		t.Fatalf("unknown-field import code = %d, want 1; stderr = %s", code, stderr.String())
	}

	bundlePath := filepath.Join(t.TempDir(), "example.bundle.json")
	if err := os.WriteFile(bundlePath, first.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	destinationRoot := filepath.Join(t.TempDir(), "destination")
	destination := instance.NewLayout(destinationRoot)
	if err := os.MkdirAll(destination.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	sourceLayout := instance.NewLayout(root)
	instanceConfig, err := os.ReadFile(sourceLayout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination.ConfigFile(), instanceConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestData, err := os.ReadFile(filepath.Join(sourceLayout.ConfigDir(), "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest apiv1.Manifest
	if err := yaml.UnmarshalStrict(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Spec.Gaggles = nil
	manifestData, err = yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination.ConfigDir(), "manifest.yaml"), manifestData, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	stderr.Reset()
	if code := runGaggleImport([]string{"--name", "copied-example", bundlePath, destinationRoot}, &stdout, &stderr); code != 0 {
		t.Fatalf("import code = %d, stderr = %s", code, stderr.String())
	}
	set, report, err := instance.LoadConfigDir(destination.ConfigDir())
	if err != nil {
		t.Fatalf("load imported config: %v report=%+v", err, report)
	}
	found := false
	for _, gaggle := range set.Gaggles {
		found = found || gaggle.Name == "copied-example"
	}
	if !found {
		t.Fatal("CLI import did not create copied-example")
	}

	manifestPath := filepath.Join(destination.ConfigDir(), "manifest.yaml")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runGaggleImport([]string{"--name", "copied-example", bundlePath, destinationRoot}, &stdout, &stderr); code != 1 {
		t.Fatalf("conflicting import code = %d, want 1; stderr = %s", code, stderr.String())
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("conflicting CLI import changed the destination manifest")
	}
}
