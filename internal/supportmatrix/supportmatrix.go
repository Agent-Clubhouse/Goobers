// Package supportmatrix declares the version-support surface a build of goobers
// claims: DSL versions and lifecycle levels, the minimum Go toolchain, and the
// OS/arch targets it is built and exercised on (#862, DVL-2).
//
// The matrix is host-declared — build-time constants maintained alongside the
// code, not probed at runtime. It includes the DSL version lifecycle as well as
// toolchain and platform support. runVersions renders it (human and --json).
package supportmatrix

import (
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Tier is the level of support a platform target carries.
type Tier string

const (
	// TierSupported means the target is built and tested on every change and is
	// a release gate — a bug there blocks a release.
	TierSupported Tier = "supported"
	// TierExperimental means the target builds and is exercised, but is not a
	// release gate; support is best-effort.
	TierExperimental Tier = "experimental"
)

// Level is the lifecycle support level carried by a DSL version.
type Level string

// DSL version lifecycle levels.
const (
	LevelPreview     Level = "preview"
	LevelSupported   Level = "supported"
	LevelDeprecated  Level = "deprecated"
	LevelUnsupported Level = "unsupported"
)

const (
	// CurrentDSLVersion is the stable language version used for transitional
	// unpinned workflows.
	CurrentDSLVersion = "1.4"
	// NextDSLVersion is the copy-forward language version with its own
	// interpreter and semantics.
	NextDSLVersion = "2.0"
)

// SupportTransition records when a DSL version entered one lifecycle level.
type SupportTransition struct {
	Level        Level  `json:"level"`
	SinceVersion string `json:"sinceVersion"`
}

// VersionSupport describes the host's lifecycle contract for one DSL version.
type VersionSupport struct {
	Level            Level               `json:"level"`
	UnsupportedAfter string              `json:"unsupportedAfter,omitempty"`
	Replacement      string              `json:"replacement,omitempty"`
	History          []SupportTransition `json:"history"`
}

// SupportMatrix is the host-declared DSL version support surface.
type SupportMatrix map[string]VersionSupport

// Version is one stable, ordered row of a SupportMatrix.
type Version struct {
	Version          string              `json:"version"`
	Level            Level               `json:"level"`
	UnsupportedAfter string              `json:"unsupportedAfter,omitempty"`
	Replacement      string              `json:"replacement,omitempty"`
	History          []SupportTransition `json:"history"`
}

var dslVersions = mustSupportMatrix(SupportMatrix{
	// DSL 1.4 is deprecated (#2700, epic #2695): every shipped, reference,
	// and example workflow now pins 2.0, and 2.0 is a verified strict
	// superset of 1.4. Deprecation takes effect with the first tagged
	// release; the version stays loadable (with a DVL020 warning naming
	// `goobers fix --to 2.0`) until the support window closes.
	//
	// UNSUPPORTED AT v0.5.0, NOT v0.2.0. Two policy constants apply, and the
	// stricter one governs: MinimumDeprecatedMinorReleases (1) is the minimum
	// deprecation period, but MinimumSupportWindowMinorReleases (3) is how long
	// a SUPERSEDED SUPPORTED version must stay loadable. 2.0 supersedes 1.4 in
	// v0.2.0, so declaring 1.4 unsupported in that same release leaves a
	// zero-release window and ValidateSupportPolicy rejects it:
	//
	//	DSL version "1.4" has unsupported release "v0.2.0" fewer than 3 minor
	//	releases after DSL version "2.0" superseded it in "v0.2.0"
	//
	// This reddened main and every open PR the moment v0.2.0 resolved as the
	// release baseline. v0.5.0 is the first release that satisfies the window.
	CurrentDSLVersion: {
		Level:            LevelDeprecated,
		Replacement:      NextDSLVersion,
		UnsupportedAfter: "v0.5.0",
		History: []SupportTransition{
			{Level: LevelSupported, SinceVersion: initialSupportVersion},
			{Level: LevelDeprecated, SinceVersion: "v0.1.0"},
		},
	},
	NextDSLVersion: {
		Level: LevelSupported,
		History: []SupportTransition{
			{Level: LevelSupported, SinceVersion: initialSupportVersion},
		},
	},
})

// Lookup returns the support declaration for a DSL version.
func (m SupportMatrix) Lookup(version string) (VersionSupport, bool) {
	support, ok := m[version]
	return cloneVersionSupport(support), ok
}

// Versions returns the matrix in numeric major/minor order.
func (m SupportMatrix) Versions() []Version {
	versions := make([]Version, 0, len(m))
	for version, support := range m {
		versions = append(versions, Version{
			Version:          version,
			Level:            support.Level,
			UnsupportedAfter: support.UnsupportedAfter,
			Replacement:      support.Replacement,
			History:          slices.Clone(support.History),
		})
	}
	sort.Slice(versions, func(i, j int) bool {
		leftMajor, leftMinor, leftOK := parseDSLVersion(versions[i].Version)
		rightMajor, rightMinor, rightOK := parseDSLVersion(versions[j].Version)
		if leftOK != rightOK {
			return leftOK
		}
		if !leftOK {
			return versions[i].Version < versions[j].Version
		}
		if leftMajor != rightMajor {
			return leftMajor < rightMajor
		}
		return leftMinor < rightMinor
	})
	return versions
}

// NewestSupported returns the newest DSL version the matrix declares
// LevelSupported, using Versions()'s numeric major/minor order. Callers that
// must pick a version for an object with no pin of its own (a workflow-less
// gaggle or goober, #3297) derive it from here rather than from
// CurrentDSLVersion: the transitional default is deprecated, and resolving an
// unpinned object there would fail validation the moment it turns unsupported
// — with no dslVersion field on those specs for the author to act on. ok is
// false when no version is currently LevelSupported, a state
// ValidateSupportPolicy never produces but one a caller must not paper over
// with a guess.
func (m SupportMatrix) NewestSupported() (version string, ok bool) {
	versions := m.Versions()
	for i := len(versions) - 1; i >= 0; i-- {
		if versions[i].Level == LevelSupported {
			return versions[i].Version, true
		}
	}
	return "", false
}

// GetDSL returns a copy of the compiled-in DSL SupportMatrix.
func GetDSL() SupportMatrix {
	out := make(SupportMatrix, len(dslVersions))
	for version, support := range dslVersions {
		out[version] = cloneVersionSupport(support)
	}
	return out
}

func cloneVersionSupport(support VersionSupport) VersionSupport {
	support.History = slices.Clone(support.History)
	return support
}

// CompareDSLVersions orders two DSL version strings by numeric major then
// minor — the same ordering Versions uses. ok is false when either operand is
// not a well-formed "<major>.<minor>" version; callers own the fail-closed
// (or fail-loud) posture instead of this package guessing an order.
func CompareDSLVersions(left, right string) (order int, ok bool) {
	leftMajor, leftMinor, leftOK := parseDSLVersion(left)
	rightMajor, rightMinor, rightOK := parseDSLVersion(right)
	if !leftOK || !rightOK {
		return 0, false
	}
	if leftMajor != rightMajor {
		if leftMajor < rightMajor {
			return -1, true
		}
		return 1, true
	}
	if leftMinor != rightMinor {
		if leftMinor < rightMinor {
			return -1, true
		}
		return 1, true
	}
	return 0, true
}

func parseDSLVersion(version string) (major, minor int, ok bool) {
	majorText, minorText, found := strings.Cut(version, ".")
	if !found || majorText == "" || minorText == "" || strings.Contains(minorText, ".") {
		return 0, 0, false
	}
	major, err := strconv.Atoi(majorText)
	if err != nil || major < 0 {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(minorText)
	if err != nil || minor < 0 {
		return 0, 0, false
	}
	return major, minor, true
}

// Platform is a single OS/arch target in the support matrix. OS and Arch use Go's
// GOOS/GOARCH spelling so they compare directly against runtime.GOOS/GOARCH.
type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	Tier Tier   `json:"tier"`
}

// minGoVersion is the minimum Go toolchain this build of goobers supports. It
// mirrors the `go` directive in go.mod (the language version the module targets);
// TestMinGoVersionMatchesGoMod guards the two against drift so the declared
// surface can never quietly diverge from what the module actually compiles with.
const minGoVersion = "1.26.6"

// platforms is the declared OS/arch support matrix. Linux and macOS are release
// gates (primary CI + the self-host runner + developer machines); Windows is
// experimental — it cross-compiles and is exercised, but Linux-only facilities
// (e.g. network:none user-namespace isolation) are not a release gate there.
//
// Maintainers update this slice as the CI matrix changes; it is the single
// host-declared source that `goobers versions` renders.
var platforms = []Platform{
	{OS: "linux", Arch: "amd64", Tier: TierSupported},
	{OS: "linux", Arch: "arm64", Tier: TierSupported},
	{OS: "darwin", Arch: "amd64", Tier: TierSupported},
	{OS: "darwin", Arch: "arm64", Tier: TierSupported},
	{OS: "windows", Arch: "amd64", Tier: TierExperimental},
}

// Matrix is the host-declared toolchain and platform support surface.
type Matrix struct {
	// MinGoVersion is the minimum Go toolchain the build compiles against,
	// matching go.mod's `go` directive.
	MinGoVersion string `json:"minGoVersion"`
	// Platforms is the declared OS/arch matrix, in a stable order.
	Platforms []Platform `json:"platforms"`
}

// Get returns the declared support matrix. The returned slice is a copy, so a
// caller cannot mutate the package's declaration.
func Get() Matrix {
	out := make([]Platform, len(platforms))
	copy(out, platforms)
	return Matrix{
		MinGoVersion: minGoVersion,
		Platforms:    out,
	}
}

// Host describes the machine this binary is running on and whether it falls
// within the declared matrix.
type Host struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// GoVersion is the Go toolchain this binary was actually built with
	// (runtime.Version(), e.g. "go1.26.0").
	GoVersion string `json:"goVersion"`
	// Supported is true when OS/arch appears in the declared matrix.
	Supported bool `json:"supported"`
	// Tier is the matched platform's tier when Supported; empty otherwise.
	Tier Tier `json:"tier,omitempty"`
}

// CurrentHost describes the running host relative to the declared matrix.
func CurrentHost() Host {
	h := Host{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		GoVersion: runtime.Version(),
	}
	for _, p := range platforms {
		if p.OS == h.OS && p.Arch == h.Arch {
			h.Supported = true
			h.Tier = p.Tier
			break
		}
	}
	return h
}

// Report is the full surface `goobers versions` renders: the declared matrix plus
// the standing of the current host within it.
type Report struct {
	MinGoVersion string     `json:"minGoVersion"`
	Platforms    []Platform `json:"platforms"`
	DSLVersions  []Version  `json:"dslVersions"`
	Host         Host       `json:"host"`
}

// NewReport composes the declared matrix with the current host.
func NewReport() Report {
	m := Get()
	return Report{
		MinGoVersion: m.MinGoVersion,
		Platforms:    m.Platforms,
		DSLVersions:  GetDSL().Versions(),
		Host:         CurrentHost(),
	}
}
