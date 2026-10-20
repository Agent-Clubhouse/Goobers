package main

import (
	"github.com/goobers/goobers/internal/instance"
	"testing"
)

func TestTrustedLocalChildrenUsesResolvedInstanceInventory(t *testing.T) {
	if trustedLocalChildren(&instance.Config{Engine: &instance.EngineConfig{}}) {
		t.Fatal("engine instance enabled local backend")
	}
	if trustedLocalChildren(nil) {
		t.Fatal("absent instance admitted")
	}
	if !trustedLocalChildren(&instance.Config{}) {
		t.Fatal("implicit self requires extra configuration")
	}
	if !trustedLocalChildren(&instance.Config{Runners: []instance.RunnerEntry{{Name: "local", Host: "self"}}}) {
		t.Fatal("explicit self refused")
	}
	for _, host := range []string{"worker-deployment", "image:example.invalid/worker:tag", "invalid host"} {
		cfg := &instance.Config{Runners: []instance.RunnerEntry{{Name: "local", Host: "self"}, {Name: "remote", Host: host}}}
		if trustedLocalChildren(cfg) {
			t.Fatalf("distributed or invalid runner %q enabled local fallback", host)
		}
	}
}
