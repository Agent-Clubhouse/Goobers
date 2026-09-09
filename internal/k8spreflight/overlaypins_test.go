package k8spreflight

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeOverlayFixture(t *testing.T, root, path, body string) {
	t.Helper()
	file := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayPinsDecodeSecretConfigAndIgnorePatchPreconditions(t *testing.T) {
	sha, old := strings.Repeat("a", 40), strings.Repeat("b", 40)
	root := t.TempDir()
	writeOverlayFixture(t, root, "kustomization.yaml", "resources: [secret.yaml]\nimages:\n- name: goobers\n  newTag: "+sha+"\npatches:\n- patch: |-\n    - op: test\n      path: /spec/template/spec/containers/0/image\n      value: goobers:"+old+"\n    - op: replace\n      path: /spec/template/spec/containers/0/image\n      value: goobers:"+sha+"\n")
	embedded := "runners:\n- host: registry.invalid/goobers:" + sha + "\n"
	writeOverlayFixture(t, root, "secret.yaml", "kind: Secret\ndata:\n  instance.yaml: "+base64.StdEncoding.EncodeToString([]byte(embedded))+"\n")
	pins, err := collectOverlayPins(root)
	if err != nil {
		t.Fatal(err)
	}
	if status, detail := compareOverlayPins(pins); status != StatusPass || len(pins) != 3 {
		t.Fatalf("pins=%+v status=%s detail=%s", pins, status, detail)
	}
	writeOverlayFixture(t, root, "secret.yaml", "kind: Secret\ndata:\n  instance.yaml: not-base64!\n")
	if _, err := collectOverlayPins(root); err == nil {
		t.Fatal("malformed Secret configuration silently ignored")
	}
}

func TestOverlayPinCheckNeverPassesUncheckedAndNamesDrift(t *testing.T) {
	if result := checkOverlayPinAgreement(context.Background(), nil, Options{}); result.Status != StatusWarn || !strings.Contains(result.Detail, "checked 0") {
		t.Fatalf("absent input silently passed: %+v", result)
	}
	root := t.TempDir()
	writeOverlayFixture(t, root, "kustomization.yaml", "resources: []\n")
	if result := checkOverlayPinAgreement(context.Background(), nil, Options{OverlayDir: root}); result.Status != StatusFail || !strings.Contains(result.Detail, "checked 0") {
		t.Fatalf("empty inspection silently passed: %+v", result)
	}
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	writeOverlayFixture(t, root, "kustomization.yaml", "resources:\n- https://github.com/Agent-Clubhouse/Goobers//deploy/reference/goobers-system?ref="+a+"\nimages:\n- name: goobers\n  newTag: "+b+"\n")
	result := checkOverlayPinAgreement(context.Background(), nil, Options{OverlayDir: root})
	if result.Status != StatusFail || !strings.Contains(result.Detail, a) || !strings.Contains(result.Detail, b) || !strings.Contains(result.Detail, "checked 2") {
		t.Fatalf("drift sites missing: %+v", result)
	}
}

func TestOverlayPinsIncludeEmbeddedConfigurationAndInlinePatches(t *testing.T) {
	root := t.TempDir()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	writeOverlayFixture(t, root, "kustomization.yaml", "resources:\n- config.yaml\npatches:\n- patch: |\n    - op: replace\n      path: /spec/template/spec/containers/0/image\n      value: registry.invalid/goobers:"+b+"\n  target:\n    kind: Deployment\n")
	writeOverlayFixture(t, root, "config.yaml", "kind: ConfigMap\ndata:\n  instance.yaml: |\n    runners:\n    - name: worker\n      host: registry.invalid/goobers-ci:"+a+"\n")
	pins, err := collectOverlayPins(root)
	if err != nil || len(pins) != 2 {
		t.Fatalf("hidden pin sites missed: %+v %v", pins, err)
	}
	if status, _ := compareOverlayPins(pins); status != StatusFail {
		t.Fatalf("embedded/patch disagreement passed: %+v", pins)
	}
}

// Mirrors the source incidents in Goobernetes-Infra/cluster/pins-test.py:
// one remote base, four image aliases, seven runner hosts, Windows suffixes,
// query parameters after ref, and comments which must not become pin sites.
func TestOverlayPinsCoverAllTwelveSourceFixtureSites(t *testing.T) {
	root := t.TempDir()
	sha := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	kustomization := "resources:\n- https://github.com/Agent-Clubhouse/Goobers//deploy/reference/goobers-system?ref=" + sha + "&timeout=180s\nimages:\n"
	for _, name := range []string{"goobers", "goobers-operator", "goobers-ci", "goobers-windows"} {
		suffix := ""
		if name == "goobers-windows" {
			suffix = "-windows"
		}
		kustomization += fmt.Sprintf("- name: %s\n  newName: example.invalid/goobers\n  newTag: %s%s\n", name, sha, suffix)
	}
	kustomization += "configMapGenerator:\n- name: worker-config\n  files:\n  - instance.yaml=instance/config\n# Precondition measured at " + other + "\n"
	writeOverlayFixture(t, root, "kustomization.yaml", kustomization)
	instance := "runners:\n"
	for i := range 7 {
		instance += fmt.Sprintf("- name: worker-%d\n  host: registry.invalid:5000/goobers-ci:%s\n", i, sha)
	}
	writeOverlayFixture(t, root, "instance/config", instance)
	writeOverlayFixture(t, root, "unreferenced.yaml", "image: registry.invalid/goobers:"+other+"\n")
	pins, err := collectOverlayPins(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 12 {
		t.Fatalf("inspected %d sites, want all 12: %+v", len(pins), pins)
	}
	windows := 0
	for _, pin := range pins {
		if pin.Commit != sha {
			t.Fatalf("comment or unrelated file became a pin: %+v", pin)
		}
		if pin.Suffix == "-windows" {
			windows++
		}
	}
	if windows != 1 {
		t.Fatalf("Windows identity suffix lost: %+v", pins)
	}
}

func TestOverlayPinsFollowLocalBasesWithoutLooping(t *testing.T) {
	root := t.TempDir()
	sha := strings.Repeat("a", 40)
	writeOverlayFixture(t, root, "overlay/kustomization.yaml", "resources:\n- ../base\n")
	writeOverlayFixture(t, root, "base/kustomization.yaml", "resources:\n- ../overlay\nimages:\n- name: goobers\n  newTag: "+sha+"\n")
	pins, err := collectOverlayPins(filepath.Join(root, "overlay"))
	if err != nil || len(pins) != 1 || pins[0].Commit != sha {
		t.Fatalf("local dependency traversal = %+v, %v", pins, err)
	}
}

func TestOverlayPinsRejectUnpinnedAndMalformedInputs(t *testing.T) {
	for _, body := range []string{
		"resources:\n- https://github.com/Agent-Clubhouse/Goobers//deploy/reference/goobers-system?ref=deadbeef\n",
		"resources:\n- https://github.com/Agent-Clubhouse/Goobers//deploy/reference/goobers-system?ref=" + strings.Repeat("a", 40) + "&ref=" + strings.Repeat("b", 40) + "\n",
		"images:\n- name: goobers\n  newTag: latest\n",
		"runners:\n- name: worker\n  host: registry.invalid/goobers-ci:dev\n",
		"resources:\n- missing-base\n",
	} {
		root := t.TempDir()
		writeOverlayFixture(t, root, "kustomization.yaml", body)
		if _, err := collectOverlayPins(root); err == nil {
			t.Errorf("accepted unresolved input: %s", body)
		}
	}
}
