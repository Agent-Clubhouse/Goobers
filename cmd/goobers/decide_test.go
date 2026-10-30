package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func decideFixture(t *testing.T, mode, fallback string, yes float64) (root, state, result string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"model": "m", "answers": map[string]any{"q": map[string]any{"type": "noul", "noul": yes}}}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("DECIDE_TEST_URL", srv.URL)
	t.Setenv("DECIDE_TEST_KEY", "k")
	t.Setenv("DECIDE_TEST_MODEL", "m")
	root = t.TempDir()
	cfg := "apiVersion: goobers.dev/v1alpha1\nkind: Instance\n"
	if mode != "" {
		cfg += "decisionGate:\n  mode: " + mode + "\n  baseURLEnv: DECIDE_TEST_URL\n  keyEnv: DECIDE_TEST_KEY\n  modelEnv: DECIDE_TEST_MODEL\n  fallback: " + fallback + "\n"
	}
	if err := os.WriteFile(instance.NewLayout(root).ConfigFile(), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	state = filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state, []byte(`{"summary":"done"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	result = filepath.Join(t.TempDir(), "out.json")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", result)
	return root, state, result
}

func decideOutput(t *testing.T, result string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDecideEnforceThresholds(t *testing.T) {
	for _, tc := range []struct {
		yes  float64
		want string
	}{{0.97, "yes"}, {0.02, "no"}, {0.5, "uncertain"}} {
		root, state, result := decideFixture(t, "enforce", "agent", tc.yes)
		code, _, stderr := runArgs(t, "decide", "--name", "q", "--question", "Is it complete?", "--state-file", state, root)
		if code != 0 {
			t.Fatalf("yes=%v: code %d: %s", tc.yes, code, stderr)
		}
		if got := decideOutput(t, result); got["decision"] != tc.want || got["mode"] != "enforce" {
			t.Errorf("yes=%v: got %v, want %s", tc.yes, got, tc.want)
		}
	}
}

func TestDecideShadowNeverChangesOutcome(t *testing.T) {
	root, state, result := decideFixture(t, "shadow", "agent", 0.99)
	if code, _, stderr := runArgs(t, "decide", "--name", "q", "--question", "x", "--state-file", state, root); code != 0 {
		t.Fatalf("code %d: %s", code, stderr)
	}
	if got := decideOutput(t, result); got["decision"] != "uncertain" || got["mode"] != "shadow" {
		t.Errorf("shadow must report uncertain: %v", got)
	}
}

func TestDecideUncertainWithFailFallbackExitsNonzero(t *testing.T) {
	root, state, _ := decideFixture(t, "enforce", "fail", 0.5)
	if code, _, _ := runArgs(t, "decide", "--name", "q", "--question", "x", "--state-file", state, root); code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
}

func TestDecideGateOffFailsUnlessOptional(t *testing.T) {
	root, state, result := decideFixture(t, "", "", 0)
	code, _, stderr := runArgs(t, "decide", "--name", "q", "--question", "x", "--state-file", state, root)
	if code != 1 || !strings.Contains(stderr, "decisionGate is off") {
		t.Fatalf("want clear failure, got %d: %s", code, stderr)
	}
	if code, _, stderr := runArgs(t, "decide", "--optional", "--name", "q", "--question", "x", "--state-file", state, root); code != 0 {
		t.Fatalf("optional: %d: %s", code, stderr)
	}
	if got := decideOutput(t, result); got["decision"] != "unavailable" {
		t.Errorf("got %v", got)
	}
}

func TestDecideRejectsBadInput(t *testing.T) {
	root, state, _ := decideFixture(t, "enforce", "agent", 0.9)
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o644)
	for _, args := range [][]string{
		{"--name", "bad name", "--question", "x", "--state-file", state},
		{"--name", "q", "--question", "x", "--state-file", bad},
		{"--name", "q", "--question", "x"},
	} {
		if code, _, _ := runArgs(t, append(append([]string{"decide"}, args...), root)...); code != 2 {
			t.Errorf("%v: want exit 2, got %d", args, code)
		}
	}
}
