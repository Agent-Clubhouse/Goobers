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

const (
	LevelPreview     Level = "preview"
	LevelSupported   Level = "supported"
	LevelDeprecated  Level = "deprecated"
	LevelUnsupported Level = "unsupported"
)

// CurrentDSLVersion is the language version implemented by the current
// interpreter. New interpreters add another SupportMatrix entry rather than
// changing this version's feature membership.
const CurrentDSLVersion = "1.4"

// VersionSupport describes the host's lifecycle contract for one DSL version.
type VersionSupport struct {
	Level            Level  `json:"level"`
	UnsupportedAfter string `json:"unsupportedAfter,omitempty"`
	Replacement      string `json:"replacement,omitempty"`
}

// SupportMatrix is the host-declared DSL version support surface.
type SupportMatrix map[string]VersionSupport

// Version is one stable, ordered row of a SupportMatrix.
type Version struct {
	Version          string `json:"version"`
	Level            Level  `json:"level"`
	UnsupportedAfter string `json:"unsupportedAfter,omitempty"`
	Replacement      string `json:"replacement,omitempty"`
}

var dslVersions = SupportMatrix{
	CurrentDSLVersion: {Level: LevelSupported},
}

// Lookup returns the support declaration for a DSL version.
func (m SupportMatrix) Lookup(version string) (VersionSupport, bool) {
	support, ok := m[version]
	return support, ok
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

// GetDSL returns a copy of the compiled-in DSL SupportMatrix.
func GetDSL() SupportMatrix {
	out := make(SupportMatrix, len(dslVersions))
	for version, support := range dslVersions {
		out[version] = support
	}
	return out
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
const minGoVersion = "1.26.0"

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
