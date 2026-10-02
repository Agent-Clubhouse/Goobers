package main

import (
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

// TestEngineHITLPolicyIsOptIn pins the rollback posture at its source: an
// instance that did not configure engine.hitl pins NO policy, which is
// byte-identical to every run started before the protocol existed.
func TestEngineHITLPolicyIsOptIn(t *testing.T) {
	if policy := engineHITLPolicy(&instance.Config{}); policy != nil {
		t.Fatalf("policy = %+v on an instance with no engine.hitl block, want nil", policy)
	}
	off := &instance.Config{Engine: &instance.EngineConfig{
		HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "q",
		HITL: &instance.EngineHITLConfig{Enabled: false, Window: "4h"},
	}}
	if policy := engineHITLPolicy(off); policy != nil {
		t.Fatalf("policy = %+v on a disabled engine.hitl block, want nil", policy)
	}
	on := &instance.Config{Engine: &instance.EngineConfig{
		HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "q",
		HITL: &instance.EngineHITLConfig{Enabled: true, Window: "4h", Actors: []string{"ops"}},
	}}
	policy := engineHITLPolicy(on)
	if policy == nil || !policy.Enabled {
		t.Fatalf("policy = %+v on an enabled engine.hitl block, want an enabled policy", policy)
	}
	if policy.WaitSeconds != 4*60*60 {
		t.Fatalf("policy window = %ds, want 14400", policy.WaitSeconds)
	}
	if len(policy.Actors) != 1 || policy.Actors[0] != "ops" {
		t.Fatalf("policy actors = %v, want the configured set", policy.Actors)
	}
	// An unbounded window is refused at load rather than silently defaulting.
	bad := instance.EngineHITLConfig{Enabled: true, Window: "4hr"}
	if err := bad.Validate(); err == nil {
		t.Fatal("an unparsable hold window was accepted")
	}
	if err := (instance.EngineHITLConfig{Enabled: true, Window: "-1h"}).Validate(); err == nil {
		t.Fatal("a negative hold window was accepted")
	}
}
