package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func decideCIFixture(t *testing.T, mode, choice string, prob float64) (root, result string, requests *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State json.RawMessage `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, string(body.State))
		dist := map[string]float64{"flaky": 0, "real": 0, "infra": 0, "environment": 0}
		for k := range dist {
			dist[k] = (1 - prob) / 3
		}
		dist[choice] = prob
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "m", "answers": map[string]any{
			"ci_failure_class": map[string]any{"type": "choice", "choice": choice, "probabilities": dist, "confidence": 0.95},
		}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("DECIDE_TEST_URL", srv.URL)
	t.Setenv("DECIDE_TEST_KEY", "k")
	t.Setenv("DECIDE_TEST_MODEL", "m")
	root = t.TempDir()
	cfg := "apiVersion: goobers.dev/v1alpha1\nkind: Instance\n"
	if mode != "" {
		cfg += "decisionGate:\n  mode: " + mode + "\n  baseURLEnv: DECIDE_TEST_URL\n  keyEnv: DECIDE_TEST_KEY\n  modelEnv: DECIDE_TEST_MODEL\n  fallback: agent\n"
	}
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	result = filepath.Join(t.TempDir(), "out.json")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", result)
	return root, result, &seen
}

func writeCILog(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ci.log")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDecideCIShadowRecordsButReportsUnsure(t *testing.T) {
	root, result, seen := decideCIFixture(t, "shadow", "flaky", 0.97)
	log := writeCILog(t, "--- FAIL: TestRace\npassword=hunter2hunter2\n")
	if code, _, stderr := runArgs(t, "decide", "--ci-log", log, "--run-key", "run-1", root); code != 0 || strings.Contains(stderr, "failed") {
		t.Fatalf("code %d: %s", code, stderr)
	}
	got := decideOutput(t, result)
	if got["class"] != "unsure" || got["predicted"] != "flaky" || got["source"] != "model" || got["mode"] != "shadow" {
		t.Errorf("shadow must report unsure and record the prediction: %v", got)
	}
	if len(*seen) != 1 || strings.Contains((*seen)[0], "hunter2") {
		t.Errorf("model request leaked the secret or was not made once: %v", *seen)
	}
	if code, _, stderr := runArgs(t, "decide", "--ci-outcome", "real", "--run-key", "run-1", root); code != 0 {
		t.Fatalf("outcome code %d: %s", code, stderr)
	}
	if code, _, stderr := runArgs(t, "decide", "--ci-matrix", root); code != 0 {
		t.Fatalf("matrix code %d: %s", code, stderr)
	}
	m := decideOutput(t, result)
	if m["modelJudged"] != "1" || m["model.flaky>real"] != "1" || m["wronglySkipped"] != "1" || m["modelAccuracy"] != "" {
		t.Errorf("matrix = %v", m)
	}
}

func TestDecideCIEnforceCodeFactsSkipModel(t *testing.T) {
	root, result, seen := decideCIFixture(t, "enforce", "real", 0.99)
	log := writeCILog(t, "dial tcp: read: ECONNRESET")
	if code, _, stderr := runArgs(t, "decide", "--ci-log", log, root); code != 0 {
		t.Fatalf("code %d: %s", code, stderr)
	}
	if got := decideOutput(t, result); got["class"] != "infra" || got["source"] != "code" {
		t.Errorf("got %v", got)
	}
	if len(*seen) != 0 {
		t.Errorf("model called for a code-decided fact")
	}
}

func TestDecideCIGateOffAndBadInput(t *testing.T) {
	root, result, _ := decideCIFixture(t, "", "real", 0.99)
	log := writeCILog(t, "boom")
	if code, _, stderr := runArgs(t, "decide", "--ci-log", log, root); code != 1 || !strings.Contains(stderr, "decisionGate is off") {
		t.Errorf("off without --optional: code=%d stderr=%q", code, stderr)
	}
	if code, _, _ := runArgs(t, "decide", "--optional", "--ci-log", log, root); code != 0 {
		t.Fatalf("optional off should exit 0")
	}
	if got := decideOutput(t, result); got["class"] != "unsure" || got["unavailable"] != "true" {
		t.Errorf("got %v", got)
	}
	for _, args := range [][]string{
		{"--ci-outcome", "unsure", "--run-key", "k"},
		{"--ci-outcome", "real"},
		{"--ci-log", log, "--ci-matrix"},
		{"--ci-matrix", "--name", "q"},
		{"--ci-log", filepath.Join(t.TempDir(), "missing.log")},
	} {
		if code, _, _ := runArgs(t, append(append([]string{"decide"}, args...), root)...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
}
