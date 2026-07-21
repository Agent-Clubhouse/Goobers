package toolchain

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestSplitToken(t *testing.T) {
	cases := []struct{ token, family, constraint string }{
		{"dotnet@8", "dotnet", "8"},
		{"dotnet@8.0.412", "dotnet", "8.0.412"},
		{"os=windows", "os", "windows"},
		{"xcode", "xcode", ""},
		{"node@20", "node", "20"},
	}
	for _, c := range cases {
		fam, con := splitToken(c.token)
		if fam != c.family || con != c.constraint {
			t.Errorf("splitToken(%q) = (%q,%q), want (%q,%q)", c.token, fam, con, c.family, c.constraint)
		}
	}
}

func TestVersionSatisfies(t *testing.T) {
	cases := []struct {
		want, got string
		ok        bool
	}{
		{"8", "8.0.412", true},
		{"8.0", "8.0.412", true},
		{"8.0.412", "8.0.412", true},
		{"8.1", "8.0.412", false},
		{"9", "8.0.412", false},
		{"8.0.5", "8.0", false}, // constraint more specific than found
		{"20", "20.11.1", true},
		{"1.26", "1.26.0", true},
	}
	for _, c := range cases {
		if got := versionSatisfies(c.want, c.got); got != c.ok {
			t.Errorf("versionSatisfies(want=%q, got=%q) = %v, want %v", c.want, c.got, got, c.ok)
		}
	}
}

func TestParsers(t *testing.T) {
	cases := []struct {
		name  string
		parse func(string) (string, error)
		in    string
		out   string
	}{
		{"dotnet", parseFirstLine, "8.0.412\n", "8.0.412"},
		{"node", parseNodeVersion, "v20.11.1\n", "20.11.1"},
		{"python", parseLastField, "Python 3.11.4\n", "3.11.4"},
		{"go", parseGoVersion, "go version go1.26.0 darwin/arm64\n", "1.26.0"},
	}
	for _, c := range cases {
		got, err := c.parse(c.in)
		if err != nil {
			t.Errorf("%s parse(%q) error: %v", c.name, c.in, err)
			continue
		}
		if got != c.out {
			t.Errorf("%s parse(%q) = %q, want %q", c.name, c.in, got, c.out)
		}
	}
}

// fakeExec returns canned output/errors keyed by binary name.
func fakeExec(outputs map[string]string, errs map[string]error) ExecFunc {
	return func(_ context.Context, name string, _ ...string) (string, error) {
		if err, ok := errs[name]; ok {
			return "", err
		}
		return outputs[name], nil
	}
}

func newTestVerifier(run ExecFunc, goos string) *Verifier {
	v := &Verifier{run: run, goos: goos}
	v.probes = map[string]Prober{
		"dotnet": commandProber{bin: "dotnet", args: []string{"--version"}, parse: parseFirstLine},
		"node":   commandProber{bin: "node", args: []string{"--version"}, parse: parseNodeVersion},
		"go":     commandProber{bin: "go", args: []string{"version"}, parse: parseGoVersion},
		"os":     osProber{goos: func() string { return v.goos }},
	}
	return v
}

func TestVerifyEmptyIsNoOp(t *testing.T) {
	v := newTestVerifier(fakeExec(nil, map[string]error{"dotnet": fmt.Errorf("should not run")}), "linux")
	if err := v.Verify(context.Background(), nil); err != nil {
		t.Errorf("empty requirement should be a no-op, got %v", err)
	}
}

func TestVerifySatisfied(t *testing.T) {
	v := newTestVerifier(fakeExec(map[string]string{
		"dotnet": "8.0.412\n",
		"go":     "go version go1.26.0 linux/amd64\n",
	}, nil), "linux")
	if err := v.Verify(context.Background(), []string{"dotnet@8", "go@1.26", "os=linux"}); err != nil {
		t.Errorf("all satisfied, got error: %v", err)
	}
}

func TestVerifyMissingBinaryFailsClosed(t *testing.T) {
	v := newTestVerifier(fakeExec(nil, map[string]error{
		"dotnet": fmt.Errorf(`exec: "dotnet": executable file not found in $PATH`),
	}), "linux")
	err := v.Verify(context.Background(), []string{"dotnet@8"})
	if err == nil {
		t.Fatal("expected preflight failure for missing dotnet")
	}
	if !strings.Contains(err.Error(), "dotnet@8") || !strings.Contains(err.Error(), "not runnable") {
		t.Errorf("diagnostic should name the token and the cause, got: %v", err)
	}
}

func TestVerifyWrongVersionFailsClosed(t *testing.T) {
	v := newTestVerifier(fakeExec(map[string]string{"dotnet": "6.0.428\n"}, nil), "linux")
	err := v.Verify(context.Background(), []string{"dotnet@8"})
	if err == nil {
		t.Fatal("expected preflight failure for wrong dotnet version")
	}
	if !strings.Contains(err.Error(), "6.0.428") || !strings.Contains(err.Error(), "requires 8") {
		t.Errorf("diagnostic should show found-vs-required, got: %v", err)
	}
}

func TestVerifyOSMismatchFailsClosed(t *testing.T) {
	v := newTestVerifier(fakeExec(nil, nil), "darwin")
	err := v.Verify(context.Background(), []string{"os=windows"})
	if err == nil {
		t.Fatal("expected os mismatch failure")
	}
	if !strings.Contains(err.Error(), "darwin") || !strings.Contains(err.Error(), "windows") {
		t.Errorf("diagnostic should show host-vs-required OS, got: %v", err)
	}
}

func TestVerifyUnprobeableFamilyIsSkipped(t *testing.T) {
	// xcode/netfx have no registered probe: not verified here (schedule-time
	// match already gated membership), so they never fail preflight.
	v := newTestVerifier(fakeExec(nil, nil), "darwin")
	if err := v.Verify(context.Background(), []string{"xcode", "netfx@4.8"}); err != nil {
		t.Errorf("unprobeable families should be skipped, got: %v", err)
	}
}

func TestVerifyAggregatesMultipleProblems(t *testing.T) {
	v := newTestVerifier(fakeExec(
		map[string]string{"dotnet": "6.0.0\n"},
		map[string]error{"node": fmt.Errorf("not found")},
	), "linux")
	err := v.Verify(context.Background(), []string{"dotnet@8", "node@20", "os=windows"})
	if err == nil {
		t.Fatal("expected aggregated failure")
	}
	for _, want := range []string{"dotnet@8", "node@20", "os=windows"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated diagnostic missing %q:\n%v", want, err)
		}
	}
}

func TestVerifyDeduplicatesTokens(t *testing.T) {
	calls := 0
	run := func(_ context.Context, name string, _ ...string) (string, error) {
		calls++
		return "8.0.1\n", nil
	}
	v := newTestVerifier(run, "linux")
	if err := v.Verify(context.Background(), []string{"dotnet@8", "dotnet@8"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("probe ran %d times for a duplicated token, want 1", calls)
	}
}

// TestDefaultVerifierProbesGo exercises the real exec path against the Go
// toolchain, which is always present in CI — a single non-hermetic smoke test
// proving execWithBaseEnv + the go probe agree with the running toolchain.
func TestDefaultVerifierProbesGo(t *testing.T) {
	v := DefaultVerifier()
	if err := v.Verify(context.Background(), []string{"os=" + v.goos}); err != nil {
		t.Errorf("os probe against own GOOS should pass: %v", err)
	}
	// go is on PATH in CI; a bogus future major must fail closed.
	if err := v.Verify(context.Background(), []string{"go@99"}); err == nil {
		t.Error("expected go@99 to fail against the real toolchain")
	}
}
