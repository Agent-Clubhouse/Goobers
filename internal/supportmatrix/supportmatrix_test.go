package supportmatrix

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGetDeclaresPlatformsAndMinGo(t *testing.T) {
	m := Get()
	if m.MinGoVersion == "" {
		t.Error("MinGoVersion is empty")
	}
	if len(m.Platforms) == 0 {
		t.Fatal("no platforms declared")
	}
	seen := map[string]bool{}
	for _, p := range m.Platforms {
		if p.OS == "" || p.Arch == "" {
			t.Errorf("platform has empty OS/arch: %+v", p)
		}
		switch p.Tier {
		case TierSupported, TierExperimental:
		default:
			t.Errorf("platform %s/%s has invalid tier %q", p.OS, p.Arch, p.Tier)
		}
		key := p.OS + "/" + p.Arch
		if seen[key] {
			t.Errorf("duplicate platform %s", key)
		}
		seen[key] = true
	}
	// The tiers we develop and gate on must be present.
	for _, want := range []string{"linux/amd64", "darwin/arm64"} {
		if !seen[want] {
			t.Errorf("expected %s in the support matrix", want)
		}
	}
}

func TestGetReturnsCopy(t *testing.T) {
	m := Get()
	if len(m.Platforms) == 0 {
		t.Fatal("no platforms")
	}
	m.Platforms[0].Tier = "mutated"
	if Get().Platforms[0].Tier == "mutated" {
		t.Error("Get() exposed the package's backing slice; a caller mutated the declaration")
	}
}

func TestCurrentHostReflectsRuntime(t *testing.T) {
	h := CurrentHost()
	if h.OS != runtime.GOOS || h.Arch != runtime.GOARCH {
		t.Errorf("host = %s/%s, want %s/%s", h.OS, h.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if h.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", h.GoVersion, runtime.Version())
	}
	// Supported ⟺ the host appears in the matrix, and a supported host carries a
	// tier while an unsupported one does not.
	var inMatrix bool
	for _, p := range Get().Platforms {
		if p.OS == h.OS && p.Arch == h.Arch {
			inMatrix = true
		}
	}
	if h.Supported != inMatrix {
		t.Errorf("Supported = %v, but host in matrix = %v", h.Supported, inMatrix)
	}
	if h.Supported && h.Tier == "" {
		t.Error("supported host has no tier")
	}
	if !h.Supported && h.Tier != "" {
		t.Errorf("unsupported host carries tier %q", h.Tier)
	}
}

func TestNewReportComposesMatrixAndHost(t *testing.T) {
	r := NewReport()
	if r.MinGoVersion != Get().MinGoVersion {
		t.Errorf("report MinGoVersion = %q, want %q", r.MinGoVersion, Get().MinGoVersion)
	}
	if len(r.Platforms) != len(Get().Platforms) {
		t.Errorf("report platforms = %d, want %d", len(r.Platforms), len(Get().Platforms))
	}
	if r.Host != CurrentHost() {
		t.Errorf("report host = %+v, want %+v", r.Host, CurrentHost())
	}
}

// TestMinGoVersionMatchesGoMod is the drift guard: the declared minimum Go
// toolchain must equal go.mod's `go` directive, so the support surface cannot
// silently diverge from the language version the module actually compiles with.
func TestMinGoVersionMatchesGoMod(t *testing.T) {
	got := goModGoDirective(t)
	if got != minGoVersion {
		t.Fatalf("minGoVersion = %q but go.mod `go` directive = %q; update supportmatrix.minGoVersion to match go.mod", minGoVersion, got)
	}
}

// goModGoDirective reads the module's go.mod (two levels up from this package)
// and returns the version on its `go` line.
func goModGoDirective(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, "go "); ok {
			return strings.TrimSpace(v)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("no `go` directive found in %s", path)
	return ""
}
