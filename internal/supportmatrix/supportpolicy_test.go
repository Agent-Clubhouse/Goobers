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

func TestSupportPolicyAllowsCoexistingVersionWithInitialHistory(t *testing.T) {
	released := GetDSL()
	current := GetDSL()
	current["2.0"] = VersionSupport{
		Level: LevelSupported,
		History: []SupportTransition{
			{Level: LevelSupported, SinceVersion: initialSupportVersion},
		},
	}

	if err := ValidateSupportPolicy(current); err != nil {
		t.Fatalf("coexisting DSL version violates support policy: %v", err)
	}
	if err := validateSupportMatrixEvolution(released, current, initialSupportVersion, nil); err != nil {
		t.Fatalf("coexisting DSL version evolution was rejected: %v", err)
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

func TestSupportPolicyAgainstReleasedMatrix(t *testing.T) {
	transition := func(level Level, version string) SupportTransition {
		return SupportTransition{Level: level, SinceVersion: version}
	}
	released := SupportMatrix{
		"1.0": {
			Level: LevelSupported,
			History: []SupportTransition{
				transition(LevelSupported, "v1.0.0"),
			},
		},
		"1.1": {
			Level: LevelSupported,
			History: []SupportTransition{
				transition(LevelSupported, "v1.1.0"),
			},
		},
	}

	tests := []struct {
		name     string
		current  SupportMatrix
		baseline string
		want     string
	}{
		{
			name: "same change deprecates and makes unsupported",
			current: SupportMatrix{
				"1.0": {
					Level: LevelUnsupported,
					History: []SupportTransition{
						transition(LevelSupported, "v1.0.0"),
						transition(LevelDeprecated, "v1.3.0"),
						transition(LevelUnsupported, "v1.4.0"),
					},
				},
				"1.1": released["1.1"],
			},
			baseline: "v1.1.0",
			want:     "must be deprecated in the latest released support matrix",
		},
		{
			name: "released history rewritten",
			current: SupportMatrix{
				"1.0": {
					Level: LevelSupported,
					History: []SupportTransition{
						transition(LevelSupported, "v1.0.1"),
					},
				},
				"1.1": released["1.1"],
			},
			baseline: "v1.1.0",
			want:     "lifecycle history must not change",
		},
		{
			name: "released version omitted",
			current: SupportMatrix{
				"1.1": released["1.1"],
			},
			baseline: "v1.1.0",
			want:     "must remain in the support matrix",
		},
		{
			name: "backdated transition appended",
			current: SupportMatrix{
				"1.0": {
					Level:            LevelDeprecated,
					UnsupportedAfter: "v1.4.0",
					History: []SupportTransition{
						transition(LevelSupported, "v1.0.0"),
						transition(LevelDeprecated, "v1.2.0"),
					},
				},
				"1.1": released["1.1"],
			},
			baseline: "v1.3.0",
			want:     `new lifecycle transition "v1.2.0" must be later than latest release "v1.3.0"`,
		},
		{
			name: "new version has backdated history",
			current: SupportMatrix{
				"1.0": released["1.0"],
				"1.1": released["1.1"],
				"1.2": {
					Level: LevelSupported,
					History: []SupportTransition{
						transition(LevelSupported, "v1.2.0"),
					},
				},
			},
			baseline: "v1.3.0",
			want:     `new lifecycle transition "v1.2.0" must be later than latest release "v1.3.0"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateSupportPolicy(test.current); err != nil {
				t.Fatalf("synthetic current matrix must satisfy its self-reported policy: %v", err)
			}
			err := validateSupportMatrixEvolution(released, test.current, test.baseline, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateSupportMatrixEvolution() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestSupportPolicyAllowsUnsupportedAfterReleasedDeprecation(t *testing.T) {
	released := SupportMatrix{
		"1.0": {
			Level:            LevelDeprecated,
			UnsupportedAfter: "v1.4.0",
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: "v1.0.0"},
				{Level: LevelDeprecated, SinceVersion: "v1.3.0"},
			},
		},
		"1.1": {
			Level: LevelSupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: "v1.1.0"},
			},
		},
	}
	current := SupportMatrix{
		"1.0": {
			Level: LevelUnsupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: "v1.0.0"},
				{Level: LevelDeprecated, SinceVersion: "v1.3.0"},
				{Level: LevelUnsupported, SinceVersion: "v1.4.0"},
			},
		},
		"1.1": released["1.1"],
	}

	if err := ValidateSupportPolicy(current); err != nil {
		t.Fatalf("current matrix violates support policy: %v", err)
	}
	if err := validateSupportMatrixEvolution(released, current, "v1.3.0", nil); err != nil {
		t.Fatalf("unsupported transition after released deprecation was rejected: %v", err)
	}
}

func TestSupportPolicyAllowsInitialDevelopmentHistory(t *testing.T) {
	current := SupportMatrix{
		"1.0": {
			Level: LevelSupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
			},
		},
	}

	if err := validateSupportMatrixEvolution(
		SupportMatrix{},
		current,
		initialSupportVersion,
		nil,
	); err != nil {
		t.Fatalf("initial development lifecycle history was rejected: %v", err)
	}
}

func TestSupportPolicyRejectsUnsupportedBeforeAnchoredDevelopmentWindow(t *testing.T) {
	released := SupportMatrix{
		"1.4": {
			Level:            LevelDeprecated,
			UnsupportedAfter: "v1.2.0",
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
				{Level: LevelDeprecated, SinceVersion: "v1.1.0"},
			},
		},
		"2.0": {
			Level: LevelSupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
			},
		},
	}
	current := SupportMatrix{
		"1.4": {
			Level: LevelUnsupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
				{Level: LevelDeprecated, SinceVersion: "v1.1.0"},
				{Level: LevelUnsupported, SinceVersion: "v1.2.0"},
			},
		},
		"2.0": released["2.0"],
	}
	firstRelease, err := parseSupportReleaseVersion("v1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}
	developmentReleases := map[string]releaseVersion{
		"1.4": firstRelease,
		"2.0": firstRelease,
	}

	err = validateSupportMatrixEvolution(released, current, "v1.1.0", developmentReleases)
	if err == nil || !strings.Contains(err.Error(), `fewer than 3 minor releases after DSL version "2.0" superseded it in "v1.0.0"`) {
		t.Fatalf("early unsupported transition error = %v, want anchored support-window failure", err)
	}
}

func TestMinorReleaseWindowDoesNotTreatDevelopmentAsZero(t *testing.T) {
	release, err := parseSupportReleaseVersion("v1.2.0", false)
	if err != nil {
		t.Fatal(err)
	}
	if atLeastMinorReleases(releaseVersion{development: true}, release, MinimumSupportWindowMinorReleases) {
		t.Fatal("development sentinel satisfied a numeric release window without an anchor")
	}
}

func TestValidateSupportPolicyForRelease(t *testing.T) {
	matrix := func(level Level) SupportMatrix {
		return SupportMatrix{
			"1.4": {
				Level:            level,
				Replacement:      "2.0",
				UnsupportedAfter: unsupportedAfterFor(level),
				History: []SupportTransition{
					{Level: LevelSupported, SinceVersion: initialSupportVersion},
					{Level: LevelDeprecated, SinceVersion: "v0.1.0"},
					{Level: LevelUnsupported, SinceVersion: "v0.5.0"},
				}[:transitionsFor(level)],
			},
			"2.0": {
				Level: LevelSupported,
				History: []SupportTransition{
					{Level: LevelSupported, SinceVersion: initialSupportVersion},
				},
			},
		}
	}

	for _, tc := range []struct {
		name    string
		matrix  SupportMatrix
		release string
		want    string
	}{
		{
			name:    "level not yet reached by the release being built",
			matrix:  matrix(LevelUnsupported),
			release: "v0.4.0",
			want:    `DSL version "1.4" declares support level "unsupported" but release "v0.4.0" only reaches "deprecated"`,
		},
		{
			name:    "pre-release tag is checked against its release line",
			matrix:  matrix(LevelUnsupported),
			release: "v0.4.0-beta.2",
			want:    `only reaches "deprecated"`,
		},
		{
			name:    "level reached by the release being built",
			matrix:  matrix(LevelUnsupported),
			release: "v0.5.0",
		},
		{
			name:    "level in effect for the whole lifecycle",
			matrix:  matrix(LevelDeprecated),
			release: "v0.4.0",
		},
		{
			name:    "untagged development build names no release",
			matrix:  matrix(LevelUnsupported),
			release: "v0.4.0-beta.2-11-g9bcc47a2e",
		},
		{
			name:    "dev sentinel names no release",
			matrix:  matrix(LevelUnsupported),
			release: initialSupportVersion,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSupportPolicyForRelease(tc.matrix, tc.release)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ValidateSupportPolicyForRelease(%s) = %v, want nil", tc.release, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateSupportPolicyForRelease(%s) = %v, want %q", tc.release, err, tc.want)
			}
		})
	}
}

func TestValidateSupportPolicyForReleaseRejectsLifecycleStartingLater(t *testing.T) {
	matrix := SupportMatrix{
		"3.0": {
			Level: LevelPreview,
			History: []SupportTransition{
				{Level: LevelPreview, SinceVersion: "v0.4.0"},
			},
		},
	}
	err := ValidateSupportPolicyForRelease(matrix, "v0.3.4")
	if err == nil || !strings.Contains(err.Error(), `lifecycle only starts in "v0.4.0", after release "v0.3.4"`) {
		t.Fatalf("unreleased lifecycle error = %v, want start-after-release failure", err)
	}
}

func TestEvolutionCarriesForwardReleasedUnsupportedAfter(t *testing.T) {
	released := SupportMatrix{
		"1.4": {
			Level:            LevelDeprecated,
			Replacement:      "2.0",
			UnsupportedAfter: "v0.5.0",
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
				{Level: LevelDeprecated, SinceVersion: "v0.1.0"},
			},
		},
		"2.0": {
			Level: LevelSupported,
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
			},
		},
	}
	firstRelease, err := parseSupportReleaseVersion("v0.1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	developmentReleases := map[string]releaseVersion{"1.4": firstRelease, "2.0": firstRelease}

	dropped := SupportMatrix{"1.4": released["1.4"], "2.0": released["2.0"]}
	pulledIn := dropped["1.4"]
	pulledIn.UnsupportedAfter = "v0.4.0"
	dropped["1.4"] = pulledIn
	err = validateSupportMatrixEvolution(released, dropped, "v0.3.3", developmentReleases)
	if err == nil || !strings.Contains(err.Error(), `drops support in "v0.4.0", before released unsupported-after release "v0.5.0"`) {
		t.Fatalf("pulled-in unsupported-after error = %v, want carry-forward failure", err)
	}

	early := SupportMatrix{
		"1.4": {
			Level:       LevelUnsupported,
			Replacement: "2.0",
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
				{Level: LevelDeprecated, SinceVersion: "v0.1.0"},
				{Level: LevelUnsupported, SinceVersion: "v0.4.0"},
			},
		},
		"2.0": released["2.0"],
	}
	err = validateSupportMatrixEvolution(released, early, "v0.3.3", developmentReleases)
	if err == nil || !strings.Contains(err.Error(), `drops support in "v0.4.0", before released unsupported-after release "v0.5.0"`) {
		t.Fatalf("early unsupported error = %v, want unsupported-after failure", err)
	}

	honored := SupportMatrix{
		"1.4": {
			Level:       LevelUnsupported,
			Replacement: "2.0",
			History: []SupportTransition{
				{Level: LevelSupported, SinceVersion: initialSupportVersion},
				{Level: LevelDeprecated, SinceVersion: "v0.1.0"},
				{Level: LevelUnsupported, SinceVersion: "v0.5.0"},
			},
		},
		"2.0": released["2.0"],
	}
	if err := validateSupportMatrixEvolution(released, honored, "v0.3.3", developmentReleases); err != nil {
		t.Fatalf("honored unsupported-after target = %v, want nil", err)
	}
}

func unsupportedAfterFor(level Level) string {
	if level == LevelDeprecated {
		return "v0.5.0"
	}
	return ""
}

func transitionsFor(level Level) int {
	if level == LevelDeprecated {
		return 2
	}
	return 3
}
