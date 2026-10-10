package decisiongate

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSettingsRejectInlineSecrets(t *testing.T) {
	ok := Settings{Mode: ModeShadow, BaseURLEnv: "SO_URL", KeyEnv: "SO_KEY", ModelEnv: "SO_MODEL", Fallback: FallbackAgent}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"sk-" + strings.Repeat("a1", 12), "Bearer abc", "has space", "a.b", ""} {
		s := ok
		s.KeyEnv = key
		if err := s.Validate(); err == nil {
			t.Errorf("keyEnv %q accepted", key)
		}
	}
	s := ok
	s.Fallback = ""
	if s.Validate() == nil {
		t.Error("fallback must be required")
	}
	if (&Settings{}).Validate() != nil || (*Settings)(nil).Validate() != nil {
		t.Error("off must validate")
	}
	g, err := (&Settings{}).Resolve(nil, nil)
	if g != nil || err != nil {
		t.Fatalf("off resolves to nothing: %v %v", g, err)
	}
}

func TestHandoffSchemasValidateAndStayInertWhileOff(t *testing.T) {
	binding := HandoffSchemaBinding{Workflow: "implementation", Stage: "query-backlog", SchemaPath: "s.json"}
	on := Settings{Mode: ModeShadow, BaseURLEnv: "U", KeyEnv: "K", ModelEnv: "M", Fallback: FallbackAgent, HandoffSchemas: []HandoffSchemaBinding{binding}}
	if err := on.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := on.ResultSchemas()["implementation"]["query-backlog"]; got != "s.json" {
		t.Fatalf("ResultSchemas = %v", on.ResultSchemas())
	}
	if off := (Settings{HandoffSchemas: on.HandoffSchemas}); off.ResultSchemas() != nil {
		t.Fatal("an off gate must bind nothing")
	}
	for name, bindings := range map[string][]HandoffSchemaBinding{
		"missing stage": {{Workflow: "implementation", SchemaPath: "s.json"}},
		"duplicate":     {binding, binding},
	} {
		s := on
		s.HandoffSchemas = bindings
		if s.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestResolveReadsEnvAndNeverEchoesKey(t *testing.T) {
	s := Settings{Mode: ModeShadow, BaseURLEnv: "U", KeyEnv: "K", ModelEnv: "M", Fallback: FallbackAgent}
	env := map[string]string{"U": "http://127.0.0.1:1", "K": "super-secret-value", "M": "m"}
	if _, err := s.Resolve(func(k string) string { return env[k] }, nil); err != nil {
		t.Fatal(err)
	}
	env["M"] = ""
	_, err := s.Resolve(func(k string) string { return env[k] }, nil)
	if err == nil || strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("err = %v", err)
	}
}

func TestSampledStable(t *testing.T) {
	if !Sampled("a", 0) || !Sampled("a", 1) {
		t.Fatal("0 and 1 mean all")
	}
	first := Sampled("run-1", 0.5)
	if second := Sampled("run-1", 0.5); first != second {
		t.Fatal("unstable")
	}
	n := 0
	for i := 0; i < 2000; i++ {
		if Sampled(string(rune('a'+i%26))+string(rune(i)), 0.25) {
			n++
		}
	}
	if n < 300 || n > 700 {
		t.Fatalf("sampled %d/2000 at 0.25", n)
	}
}

func TestShadowAndTally(t *testing.T) {
	g, _ := New(&fake{yes: 0.99}, cfg(), nil)
	var tl Tally
	tl.Add(g.Shadow(context.Background(), true, true, "corrupted"))
	g2, _ := New(&fake{yes: 0.01}, cfg(), nil)
	tl.Add(g2.Shadow(context.Background(), true, true, "corrupted"))
	tl.Add(g.Shadow(context.Background(), true, false, `{"ok":1}`))
	if tl.Total != 3 || tl.GroundSpurious != 2 || tl.CaughtSpurious != 1 || tl.MissedSpurious != 1 || tl.FalseAlarms != 1 {
		t.Fatalf("%+v", tl)
	}
}

// Guards against committing something key-shaped in this package. Long plain
// identifiers are allowed; a long token only counts if it mixes in a digit.
func TestNoKeyShapedStringsCommitted(t *testing.T) {
	prefixed := regexp.MustCompile(`(?i)(sk-[a-z0-9]{20,}|bearer\s+[a-z0-9._-]{20,})`)
	longToken := regexp.MustCompile(`(?i)[a-z0-9]{40,}`)
	digit := regexp.MustCompile(`[0-9]`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "regexp.MustCompile") {
				continue
			}
			if m := prefixed.FindString(line); m != "" {
				t.Errorf("%s: key-shaped string %q", f, m[:6]+"...")
			}
			if m := longToken.FindString(line); m != "" && digit.MatchString(m) {
				t.Errorf("%s: key-shaped token %q", f, m[:6]+"...")
			}
		}
	}
}

func TestResolveDefaultsClaimThreshold(t *testing.T) {
	s := &Settings{Mode: ModeShadow, BaseURLEnv: "B_URL", KeyEnv: "B_KEY", ModelEnv: "B_MODEL", Fallback: FallbackAgent}
	env := map[string]string{"B_URL": "http://127.0.0.1:1", "B_KEY": "k", "B_MODEL": "m"}
	g, err := s.Resolve(func(k string) string { return env[k] }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.cfg.Thresholds[ClaimQuestion]; got != DefaultClaimThreshold {
		t.Fatalf("claim threshold = %+v, want default", got)
	}
	if s.Gate.Thresholds != nil {
		t.Fatal("Resolve must not mutate the caller's settings")
	}
	s.Gate.Thresholds = map[string]Threshold{ClaimQuestion: {Accept: 0.8, Reject: 0.2}}
	g, err = s.Resolve(func(k string) string { return env[k] }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.cfg.Thresholds[ClaimQuestion]; got.Accept != 0.8 {
		t.Fatalf("explicit threshold overridden: %+v", got)
	}
}
