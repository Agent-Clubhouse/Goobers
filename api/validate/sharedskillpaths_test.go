package validate

import "testing"

func TestMissingSkillLocationsReflectPersonaScope(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ gaggle, want string }{
		{"", `"skills/implement"`},
		{"example", `"gaggles/example/skills/implement" or "skills/implement"`},
	} {
		if got := missingSkillLocations(test.gaggle, "implement"); got != test.want {
			t.Errorf("gaggle %q: locations = %s, want %s", test.gaggle, got, test.want)
		}
	}
}
