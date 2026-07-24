package supportmatrix

import (
	"strings"
	"testing"
)

func TestDSLMatrixSatisfiesSupportPolicy(t *testing.T) {
	if err := ValidateSupportPolicy(GetDSL()); err != nil {
		t.Fatalf("compiled-in DSL support matrix violates support policy: %v", err)
	}
}

func TestSupportPolicyRejectsInvalidMatrices(t *testing.T) {
	transition := func(level Level, version string) SupportTransition {
		return SupportTransition{Level: level, SinceVersion: version}
	}
	entry := func(level Level, history ...SupportTransition) VersionSupport {
		return VersionSupport{Level: level, History: history}
	}
	superseding := entry(LevelSupported, transition(LevelSupported, "v1.1.0"))

	tests := []struct {
		name   string
		matrix SupportMatrix
		want   string
	}{
		{
			name: "supported directly to unsupported",
			matrix: SupportMatrix{
				"1.0": entry(
					LevelUnsupported,
					transition(LevelSupported, "v1.0.0"),
					transition(LevelUnsupported, "v1.4.0"),
				),
				"1.1": superseding,
			},
			want: `invalid lifecycle transition "supported" -> "unsupported"`,
		},
		{
			name: "deprecated for less than one minor",
			matrix: SupportMatrix{
				"1.0": entry(
					LevelUnsupported,
					transition(LevelSupported, "v1.0.0"),
					transition(LevelDeprecated, "v1.2.0"),
					transition(LevelUnsupported, "v1.2.1"),
				),
				"1.1": superseding,
			},
			want: "must remain deprecated until a later minor release",
		},
		{
			name: "support window shorter than three minors",
			matrix: SupportMatrix{
				"1.0": entry(
					LevelUnsupported,
					transition(LevelSupported, "v1.0.0"),
					transition(LevelDeprecated, "v1.2.0"),
					transition(LevelUnsupported, "v1.3.0"),
				),
				"1.1": superseding,
			},
			want: "fewer than 3 minor releases",
		},
		{
			name: "planned support window shorter than three minors",
			matrix: SupportMatrix{
				"1.0": {
					Level:            LevelDeprecated,
					UnsupportedAfter: "v1.3.0",
					History: []SupportTransition{
						transition(LevelSupported, "v1.0.0"),
						transition(LevelDeprecated, "v1.2.0"),
					},
				},
				"1.1": superseding,
			},
			want: "fewer than 3 minor releases",
		},
		{
			name: "major bump alone does not satisfy support window",
			matrix: SupportMatrix{
				"1.0": entry(
					LevelUnsupported,
					transition(LevelSupported, "v1.8.0"),
					transition(LevelDeprecated, "v1.9.0"),
					transition(LevelUnsupported, "v2.0.0"),
				),
				"1.1": entry(LevelSupported, transition(LevelSupported, "v1.9.0")),
			},
			want: "fewer than 3 minor releases",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateSupportPolicy(test.matrix)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateSupportPolicy() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestSupportPolicyAcceptsWindowAtFloor(t *testing.T) {
	tests := map[string]VersionSupport{
		"actual unsupported transition": {
			Level: LevelUnsupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: "v1.0.0"},
				{Level: LevelDeprecated, SinceVersion: "v1.3.0"},
				{Level: LevelUnsupported, SinceVersion: "v1.4.0"},
			},
		},
		"planned unsupported transition": {
			Level:            LevelDeprecated,
			UnsupportedAfter: "v1.4.0",
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: "v1.0.0"},
				{Level: LevelDeprecated, SinceVersion: "v1.3.0"},
			},
		},
		"major boundary at floor": {
			Level: LevelUnsupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: "v1.8.0"},
				{Level: LevelDeprecated, SinceVersion: "v2.1.0"},
				{Level: LevelUnsupported, SinceVersion: "v2.2.0"},
			},
		},
	}
	for name, support := range tests {
		t.Run(name, func(t *testing.T) {
			supersededAt := "v1.1.0"
			if name == "major boundary at floor" {
				supersededAt = "v1.9.0"
			}
			matrix := SupportMatrix{
				"1.0": support,
				"1.1": {
					Level: LevelSupported,
					History: []SupportTransition{
						{Level: LevelSupported, SinceVersion: supersededAt},
					},
				},
			}
			if err := ValidateSupportPolicy(matrix); err != nil {
				t.Fatalf("support window at policy floor was rejected: %v", err)
			}
		})
	}
}
