package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	if got := decideOutput(t, result); got["decision"] != "uncertain" || got["mode"] != "shadow" || got["probability"] != "0.9900" {
		t.Errorf("shadow must report uncertain: %v", got)
	}
}

func TestDecideOptionalCriteriaRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want map[string]string
	}{
		{name: "omitted"},
		{name: "true-only", args: []string{"--true", "Supported"}, want: map[string]string{"true": "Supported"}},
		{name: "false-only", args: []string{"--false", "Unsupported"}, want: map[string]string{"false": "Unsupported"}},
		{name: "both", args: []string{"--true", "Supported", "--false", "Unsupported"}, want: map[string]string{"true": "Supported", "false": "Unsupported"}},
		{name: "explicit-empty", args: []string{"--true", "", "--false", ""}},
		{name: "empty-true", args: []string{"--true", "", "--false", "Unsupported"}, want: map[string]string{"false": "Unsupported"}},
		{name: "blank", args: []string{"--true", " \t", "--false", "\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, state, result := decideFixture(t, "shadow", "agent", 0.97)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Questions map[string]struct {
						Criteria json.RawMessage `json:"criteria"`
					} `json:"questions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode request: %v", err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				raw := request.Questions["q"].Criteria
				var criteria map[string]string
				if len(raw) != 0 {
					if err := json.Unmarshal(raw, &criteria); err != nil {
						t.Errorf("decode criteria: %v", err)
					}
				}
				if !reflect.DeepEqual(criteria, tc.want) || (tc.want == nil && len(raw) != 0) {
					t.Errorf("criteria = %s, want %v", raw, tc.want)
					http.Error(w, "criteria must omit empty values", http.StatusUnprocessableEntity)
					return
				}
				_, _ = w.Write([]byte(`{"model":"m","answers":{"q":{"type":"noul","noul":0.97}}}`))
			}))
			defer srv.Close()
			t.Setenv("DECIDE_TEST_URL", srv.URL)
			args := append([]string{"decide", "--name", "q", "--question", "Is it supported?", "--state-file", state}, tc.args...)
			code, _, stderr := runArgs(t, append(args, root)...)
			if code != 0 {
				t.Fatalf("code %d: %s", code, stderr)
			}
			got := decideOutput(t, result)
			if got["decision"] != "uncertain" || got["mode"] != "shadow" || got["probability"] != "0.9700" {
				t.Errorf("shadow must retain a real probability without enforcing: %v; stderr: %s", got, stderr)
			}
		})
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
