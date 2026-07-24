package supportmatrix

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const (
	// MinimumSupportWindowMinorReleases is the number of minor releases for
	// which a superseded supported DSL version must remain loadable.
	MinimumSupportWindowMinorReleases = 3
	// MinimumDeprecatedMinorReleases is the minimum deprecation period before a
	// DSL version may become unsupported.
	MinimumDeprecatedMinorReleases = 1

	initialSupportVersion = "dev"
)

type versionLifecycle struct {
	version          string
	dslMajor         int
	dslMinor         int
	supportedAt      releaseVersion
	unsupportedAt    releaseVersion
	hasSupportedAt   bool
	hasUnsupportedAt bool
}

// ValidateSupportPolicy checks the lifecycle history and support-window policy
// for every DSL version in matrix.
func ValidateSupportPolicy(matrix SupportMatrix) error {
	lifecycles := make([]versionLifecycle, 0, len(matrix))
	for _, version := range matrix.Versions() {
		major, minor, ok := parseDSLVersion(version.Version)
		if !ok {
			return fmt.Errorf("DSL version %q must use MAJOR.MINOR", version.Version)
		}
		lifecycle, err := validateVersionHistory(version)
		if err != nil {
			return fmt.Errorf("DSL version %q: %w", version.Version, err)
		}
		lifecycle.dslMajor = major
		lifecycle.dslMinor = minor
		lifecycles = append(lifecycles, lifecycle)
	}

	for _, lifecycle := range lifecycles {
		if !lifecycle.hasUnsupportedAt {
			continue
		}
		superseding, ok := firstSupersedingVersion(lifecycle, lifecycles)
		if !ok {
			return fmt.Errorf(
				"DSL version %q declares unsupported release %q without a newer supported DSL version to establish supersession",
				lifecycle.version,
				lifecycle.unsupportedAt.String(),
			)
		}
		if !atLeastMinorReleases(
			superseding.supportedAt,
			lifecycle.unsupportedAt,
			MinimumSupportWindowMinorReleases,
		) {
			return fmt.Errorf(
				"DSL version %q has unsupported release %q fewer than %d minor releases after DSL version %q superseded it in %q",
				lifecycle.version,
				lifecycle.unsupportedAt.String(),
				MinimumSupportWindowMinorReleases,
				superseding.version,
				superseding.supportedAt.String(),
			)
		}
	}
	return nil
}

func validateSupportMatrixEvolution(released, current SupportMatrix) error {
	for _, previous := range released.Versions() {
		candidate, ok := current.Lookup(previous.Version)
		if !ok {
			return fmt.Errorf("released DSL version %q must remain in the support matrix", previous.Version)
		}
		if len(candidate.History) < len(previous.History) ||
			!slices.Equal(candidate.History[:len(previous.History)], previous.History) {
			return fmt.Errorf("released DSL version %q lifecycle history must not change", previous.Version)
		}
	}

	for _, candidate := range current.Versions() {
		if candidate.Level != LevelUnsupported {
			continue
		}
		previous, ok := released.Lookup(candidate.Version)
		if !ok || (previous.Level != LevelDeprecated && previous.Level != LevelUnsupported) {
			return fmt.Errorf(
				"DSL version %q must be deprecated in the latest released support matrix before becoming unsupported",
				candidate.Version,
			)
		}
	}
	return nil
}

func mustSupportMatrix(matrix SupportMatrix) SupportMatrix {
	if err := ValidateSupportPolicy(matrix); err != nil {
		panic(err)
	}
	return matrix
}

func validateVersionHistory(version Version) (versionLifecycle, error) {
	lifecycle := versionLifecycle{version: version.Version}
	if len(version.History) == 0 {
		return lifecycle, fmt.Errorf("lifecycle history must not be empty")
	}

	var previousVersion releaseVersion
	for i, transition := range version.History {
		if !validLevel(transition.Level) {
			return lifecycle, fmt.Errorf("lifecycle history has unknown support level %q", transition.Level)
		}
		release, err := parseSupportReleaseVersion(transition.SinceVersion, i == 0)
		if err != nil {
			return lifecycle, fmt.Errorf("invalid lifecycle version %q: %w", transition.SinceVersion, err)
		}
		if i == 0 {
			if transition.Level != LevelPreview && transition.Level != LevelSupported {
				return lifecycle, fmt.Errorf("lifecycle must start at preview or supported, not %q", transition.Level)
			}
		} else {
			previous := version.History[i-1]
			if !validLevelTransition(previous.Level, transition.Level) {
				return lifecycle, fmt.Errorf("invalid lifecycle transition %q -> %q", previous.Level, transition.Level)
			}
			if compareReleaseVersions(previousVersion, release) >= 0 {
				return lifecycle, fmt.Errorf(
					"lifecycle version %q must follow %q",
					transition.SinceVersion,
					previous.SinceVersion,
				)
			}
			if transition.Level == LevelUnsupported &&
				!atLeastMinorReleases(previousVersion, release, MinimumDeprecatedMinorReleases) {
				return lifecycle, fmt.Errorf(
					"version deprecated in %q must remain deprecated until a later minor release before becoming unsupported in %q",
					previous.SinceVersion,
					transition.SinceVersion,
				)
			}
		}

		switch transition.Level {
		case LevelSupported:
			lifecycle.supportedAt = release
			lifecycle.hasSupportedAt = true
		case LevelUnsupported:
			lifecycle.unsupportedAt = release
			lifecycle.hasUnsupportedAt = true
		}
		previousVersion = release
	}

	current := version.History[len(version.History)-1]
	if version.Level != current.Level {
		return lifecycle, fmt.Errorf(
			"current support level %q does not match lifecycle history level %q",
			version.Level,
			current.Level,
		)
	}
	if version.Level == LevelDeprecated {
		if strings.TrimSpace(version.UnsupportedAfter) == "" {
			return lifecycle, fmt.Errorf("deprecated version must declare unsupported-after release")
		}
		unsupportedAt, err := parseSupportReleaseVersion(version.UnsupportedAfter, false)
		if err != nil {
			return lifecycle, fmt.Errorf("invalid unsupported-after release %q: %w", version.UnsupportedAfter, err)
		}
		if !atLeastMinorReleases(previousVersion, unsupportedAt, MinimumDeprecatedMinorReleases) {
			return lifecycle, fmt.Errorf(
				"version deprecated in %q must remain deprecated until a later minor release before planned unsupported release %q",
				current.SinceVersion,
				version.UnsupportedAfter,
			)
		}
		lifecycle.unsupportedAt = unsupportedAt
		lifecycle.hasUnsupportedAt = true
	}
	return lifecycle, nil
}

func firstSupersedingVersion(current versionLifecycle, lifecycles []versionLifecycle) (versionLifecycle, bool) {
	var selected versionLifecycle
	found := false
	for _, candidate := range lifecycles {
		if !candidate.hasSupportedAt ||
			candidate.dslMajor < current.dslMajor ||
			(candidate.dslMajor == current.dslMajor && candidate.dslMinor <= current.dslMinor) ||
			compareReleaseVersions(candidate.supportedAt, current.supportedAt) < 0 {
			continue
		}
		if !found ||
			compareReleaseVersions(candidate.supportedAt, selected.supportedAt) < 0 ||
			(compareReleaseVersions(candidate.supportedAt, selected.supportedAt) == 0 &&
				(candidate.dslMajor < selected.dslMajor ||
					(candidate.dslMajor == selected.dslMajor && candidate.dslMinor < selected.dslMinor))) {
			selected = candidate
			found = true
		}
	}
	return selected, found
}

func validLevel(level Level) bool {
	switch level {
	case LevelPreview, LevelSupported, LevelDeprecated, LevelUnsupported:
		return true
	default:
		return false
	}
}

func validLevelTransition(from, to Level) bool {
	switch from {
	case LevelPreview:
		return to == LevelSupported
	case LevelSupported:
		return to == LevelDeprecated
	case LevelDeprecated:
		return to == LevelUnsupported
	default:
		return false
	}
}

type releaseVersion struct {
	development bool
	major       uint64
	minor       uint64
	patch       uint64
}

func (v releaseVersion) String() string {
	if v.development {
		return initialSupportVersion
	}
	return fmt.Sprintf("v%d.%d.%d", v.major, v.minor, v.patch)
}

func parseSupportReleaseVersion(value string, allowDevelopment bool) (releaseVersion, error) {
	if value == initialSupportVersion {
		if allowDevelopment {
			return releaseVersion{development: true}, nil
		}
		return releaseVersion{}, fmt.Errorf("%q is only valid for the initial pre-release baseline", value)
	}
	if value != strings.TrimSpace(value) || !strings.HasPrefix(value, "v") {
		return releaseVersion{}, fmt.Errorf("must use vMAJOR.MINOR.PATCH")
	}
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 3 {
		return releaseVersion{}, fmt.Errorf("must use vMAJOR.MINOR.PATCH")
	}
	numbers := make([]uint64, len(parts))
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return releaseVersion{}, fmt.Errorf("must use canonical vMAJOR.MINOR.PATCH")
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return releaseVersion{}, fmt.Errorf("must use vMAJOR.MINOR.PATCH")
		}
		numbers[i] = number
	}
	return releaseVersion{major: numbers[0], minor: numbers[1], patch: numbers[2]}, nil
}

func compareReleaseVersions(left, right releaseVersion) int {
	switch {
	case left.development != right.development:
		if left.development {
			return -1
		}
		return 1
	case left.major < right.major:
		return -1
	case left.major > right.major:
		return 1
	case left.minor < right.minor:
		return -1
	case left.minor > right.minor:
		return 1
	case left.patch < right.patch:
		return -1
	case left.patch > right.patch:
		return 1
	default:
		return 0
	}
}

func atLeastMinorReleases(from, to releaseVersion, minimum uint64) bool {
	if compareReleaseVersions(from, to) >= 0 {
		return false
	}
	if to.major > from.major {
		// Each crossed major boundary advances at least one release line (the
		// new major's .0), followed by to.minor minor release lines.
		return to.major-from.major+to.minor >= minimum
	}
	return to.major == from.major && to.minor-from.minor >= minimum
}
