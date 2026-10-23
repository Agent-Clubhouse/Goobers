package pgroles

import (
	"strings"
	"testing"
)

func TestInstanceDatabase(t *testing.T) {
	for instance, want := range map[string]string{"prod": "goobers_prod", "Team-A": "goobers_team_a"} {
		if got, err := InstanceDatabase(instance); err != nil || got != want {
			t.Fatalf("InstanceDatabase(%q) = %q, %v; want %q", instance, got, err, want)
		}
	}
	for _, instance := range []string{"", "1st", "a b", "x;drop", strings.Repeat("a", 41)} {
		if _, err := InstanceDatabase(instance); err == nil {
			t.Fatalf("InstanceDatabase(%q) accepted an unsafe name", instance)
		}
	}
}

func TestRuntimeRolesAreContractRoles(t *testing.T) {
	for _, role := range []string{API, Controller} {
		if !isRole(role) {
			t.Fatalf("%s is not a contract role", role)
		}
	}
}
