package decisiongate

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/decider"
)

func entraSettings() *Settings {
	return &Settings{Mode: ModeShadow, Auth: AuthEntra, BaseURLEnv: "D_URL", ModelEnv: "D_MODEL", Fallback: FallbackAgent}
}

func TestEntraAuthNeedsNoKeyAndRejectsOne(t *testing.T) {
	s := entraSettings()
	if err := s.Validate(); err != nil {
		t.Fatalf("entra without key: %v", err)
	}
	s.KeyEnv = "D_KEY"
	if err := s.Validate(); err == nil {
		t.Fatal("keyEnv must be rejected with entra")
	}
	bad := entraSettings()
	bad.Auth = "magic"
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown auth accepted")
	}
}

func TestResolveEntraUsesTokenSource(t *testing.T) {
	old := newEntraTokenSource
	t.Cleanup(func() { newEntraTokenSource = old })
	called := false
	newEntraTokenSource = func() (func(context.Context) (string, error), error) {
		called = true
		return func(context.Context) (string, error) { return "t", nil }, nil
	}
	_ = decider.EntraScope
	env := map[string]string{"D_URL": "https://example.test", "D_MODEL": "m"}
	g, err := entraSettings().Resolve(func(k string) string { return env[k] }, nil)
	if err != nil || g == nil || !called {
		t.Fatalf("resolve: gate=%v err=%v called=%v", g, err, called)
	}
}
