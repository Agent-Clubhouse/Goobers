package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unicode/utf16"
)

const readyWSLProbeOutput = `GOOBERS_WSL_DISTRO=Ubuntu-24.04
GOOBERS_WSL_KERNEL=6.6.87.2-microsoft-standard-WSL2
GOOBERS_WSL_GOOBERS=/usr/local/bin/goobers
GOOBERS_WSL_BWRAP=/usr/bin/bwrap
GOOBERS_WSL_READY=1
`

func readyWSLDeps() wslPreflightDeps {
	return wslPreflightDeps{
		hostOS: "windows",
		lookPath: func(name string) (string, error) {
			if name != "wsl.exe" {
				return "", errors.New("unexpected executable")
			}
			return `C:\Windows\System32\wsl.exe`, nil
		},
		probe: func(_ context.Context, _ string, args []string) ([]byte, error) {
			if reflect.DeepEqual(args, []string{"--list", "--verbose"}) {
				return []byte("  NAME            STATE           VERSION\n* Ubuntu-24.04    Running         2\n"), nil
			}
			return []byte(readyWSLProbeOutput), nil
		},
		launch: func(string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
			return 0, errors.New("unexpected launch")
		},
		getwd: func() (string, error) { return `C:\goobers`, nil },
		stdin: strings.NewReader(""),
	}
}

func TestOnboardingPreflightReportsReadyWSL(t *testing.T) {
	deps := readyWSLDeps()
	var gotProbeArgs []string
	deps.probe = func(_ context.Context, executable string, args []string) ([]byte, error) {
		if executable != `C:\Windows\System32\wsl.exe` {
			t.Fatalf("probe executable = %q", executable)
		}
		if reflect.DeepEqual(args, []string{"--list", "--verbose"}) {
			return []byte("  NAME            STATE           VERSION\n* Ubuntu-24.04    Running         2\n"), nil
		}
		gotProbeArgs = append([]string(nil), args...)
		return []byte(readyWSLProbeOutput), nil
	}

	var stdout, stderr bytes.Buffer
	code := runOnboardingPreflightWith([]string{"--distro", "Ubuntu-24.04"}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	wantPrefix := []string{"--distribution", "Ubuntu-24.04", "--exec", "/bin/sh", "-lc"}
	if len(gotProbeArgs) != len(wantPrefix)+1 || !reflect.DeepEqual(gotProbeArgs[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("probe args = %q, want prefix %q plus probe script", gotProbeArgs, wantPrefix)
	}
	for _, want := range []string{
		`WSL 2 distro "Ubuntu-24.04" is ready`,
		"/usr/local/bin/goobers",
		"user + network namespaces",
		`preflight --distro "Ubuntu-24.04" --launch-wsl`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q: %q", want, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestOnboardingPreflightFailureGuidance(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "Goobers missing",
			output: "GOOBERS_WSL_DISTRO=Ubuntu\nGOOBERS_WSL_KERNEL=6.6-microsoft-standard-WSL2\nGOOBERS_WSL_ERROR=goobers\n",
			want:   "Linux `goobers` is not installed",
		},
		{
			name:   "Bubblewrap missing",
			output: "GOOBERS_WSL_DISTRO=Ubuntu\nGOOBERS_WSL_KERNEL=6.6-microsoft-standard-WSL2\nGOOBERS_WSL_GOOBERS=/bin/goobers\nGOOBERS_WSL_ERROR=bwrap\n",
			want:   "sudo apt-get install bubblewrap",
		},
		{
			name: "Namespaces blocked",
			output: "GOOBERS_WSL_DISTRO=Ubuntu\nGOOBERS_WSL_KERNEL=6.6-microsoft-standard-WSL2\n" +
				"GOOBERS_WSL_GOOBERS=/bin/goobers\nGOOBERS_WSL_BWRAP=/usr/bin/bwrap\n" +
				"GOOBERS_WSL_ERROR=namespaces\nbwrap: No permissions to create new namespace\n",
			want: "No permissions to create new namespace",
		},
		{
			name:   "No installed distro",
			output: "There is no distribution with the supplied name.\n",
			want:   "install/select a WSL 2 distro",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := readyWSLDeps()
			deps.probe = func(_ context.Context, _ string, args []string) ([]byte, error) {
				if reflect.DeepEqual(args, []string{"--list", "--verbose"}) {
					return []byte("  NAME        STATE      VERSION\n* Ubuntu      Running    2\n"), nil
				}
				return []byte(tt.output), errors.New("exit status 1")
			}
			var stdout, stderr bytes.Buffer
			if code := runOnboardingPreflightWith(nil, &stdout, &stderr, deps); code != 1 {
				t.Fatalf("code = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.want)
			}
			if strings.Contains(stderr.String(), "ready for full") {
				t.Fatalf("failure reported as ready: %q", stderr.String())
			}
		})
	}
}

func TestOnboardingPreflightRequiresWSLInstallation(t *testing.T) {
	deps := readyWSLDeps()
	deps.lookPath = func(string) (string, error) { return "", errors.New("not found") }

	var stdout, stderr bytes.Buffer
	if code := runOnboardingPreflightWith(nil, &stdout, &stderr, deps); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "wsl.exe --install") {
		t.Fatalf("stderr = %q, want WSL installation guidance", stderr.String())
	}
}

func TestOnboardingPreflightRejectsWSL1BeforeRuntimeProbe(t *testing.T) {
	deps := readyWSLDeps()
	probes := 0
	deps.probe = func(_ context.Context, _ string, args []string) ([]byte, error) {
		probes++
		if !reflect.DeepEqual(args, []string{"--list", "--verbose"}) {
			t.Fatalf("unexpected runtime probe for WSL 1: %q", args)
		}
		return []byte("  NAME        STATE      VERSION\n* Ubuntu      Stopped    1\n"), nil
	}

	var stdout, stderr bytes.Buffer
	if code := runOnboardingPreflightWith(nil, &stdout, &stderr, deps); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if probes != 1 {
		t.Fatalf("probe calls = %d, want 1", probes)
	}
	if !strings.Contains(stderr.String(), `wsl.exe --set-version "Ubuntu" 2`) {
		t.Fatalf("stderr = %q, want WSL 2 conversion guidance", stderr.String())
	}
}

func TestParseWSLDistributionAcceptsCustomWSL2Kernel(t *testing.T) {
	output := []byte("  NAME                  STATE       VERSION\n* Custom Kernel Distro  Running     2\n")
	got, err := parseWSLDistribution(output, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.name != "Custom Kernel Distro" || got.version != "2" {
		t.Fatalf("distribution = %#v", got)
	}
}

func TestOnboardingPreflightDecodesNativeWSLDiagnostics(t *testing.T) {
	deps := readyWSLDeps()
	deps.probe = func(context.Context, string, []string) ([]byte, error) {
		return utf16LE("There is no distribution with the supplied name.\r\n"), errors.New("exit status 1")
	}

	var stdout, stderr bytes.Buffer
	if code := runOnboardingPreflightWith(nil, &stdout, &stderr, deps); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "There is no distribution with the supplied name.") {
		t.Fatalf("stderr = %q, want decoded WSL diagnostic", stderr.String())
	}
	if strings.ContainsRune(stderr.String(), '\x00') {
		t.Fatalf("stderr contains UTF-16 NUL bytes: %q", stderr.String())
	}
}

func utf16LE(value string) []byte {
	units := utf16.Encode([]rune(value))
	output := make([]byte, 2+len(units)*2)
	output[0] = 0xff
	output[1] = 0xfe
	for i, unit := range units {
		binary.LittleEndian.PutUint16(output[2+i*2:], unit)
	}
	return output
}

func TestOnboardingPreflightLaunchesLinuxGoobersAndPreservesExitCode(t *testing.T) {
	deps := readyWSLDeps()
	var gotExecutable string
	var gotArgs []string
	deps.launch = func(executable string, args []string, _ io.Reader, _, _ io.Writer) (int, error) {
		gotExecutable = executable
		gotArgs = append([]string(nil), args...)
		return 3, nil
	}

	var stdout, stderr bytes.Buffer
	code := runOnboardingPreflightWith(
		[]string{"--distro", "Ubuntu-24.04", "--launch-wsl", "--", "run", "implementation", "."},
		&stdout,
		&stderr,
		deps,
	)
	if code != 3 {
		t.Fatalf("code = %d, want forwarded exit code 3", code)
	}
	if gotExecutable != `C:\Windows\System32\wsl.exe` {
		t.Fatalf("executable = %q", gotExecutable)
	}
	wantArgs := []string{
		"--distribution", "Ubuntu-24.04",
		"--cd", `C:\goobers`,
		"--exec", "/bin/sh", "-lc", `exec "$@"`, "goobers-wsl",
		"/usr/local/bin/goobers",
		"run", "implementation", ".",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("launch args = %#v, want %#v", gotArgs, wantArgs)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestOnboardingPreflightRejectsUnsupportedHostAndInvalidLaunch(t *testing.T) {
	t.Run("non-Windows-non-Linux", func(t *testing.T) {
		deps := readyWSLDeps()
		deps.hostOS = "darwin"
		var stdout, stderr bytes.Buffer
		if code := runOnboardingPreflightWith(nil, &stdout, &stderr, deps); code != 2 {
			t.Fatalf("code = %d, want 2", code)
		}
		if !strings.Contains(stderr.String(), "only available on Windows") {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})

	t.Run("launch needs command", func(t *testing.T) {
		deps := readyWSLDeps()
		var stdout, stderr bytes.Buffer
		if code := runOnboardingPreflightWith([]string{"--launch-wsl"}, &stdout, &stderr, deps); code != 2 {
			t.Fatalf("code = %d, want 2", code)
		}
		if !strings.Contains(stderr.String(), "Usage: goobers preflight") {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})
}

// TestOnboardingPreflightLinuxCapabilityReport proves #4267's fix: `goobers
// preflight` on Linux reports the unprivileged-userns capability directly
// instead of unconditionally exiting 2 as if Linux preflight didn't exist.
func TestOnboardingPreflightLinuxCapabilityReport(t *testing.T) {
	t.Run("capable host", func(t *testing.T) {
		deps := readyWSLDeps()
		deps.hostOS = "linux"
		deps.probeNetworkNone = func(context.Context) error { return nil }
		var stdout, stderr bytes.Buffer
		if code := runOnboardingPreflightWith(nil, &stdout, &stderr, deps); code != 0 {
			t.Fatalf("code = %d, want 0, stderr = %q", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "OK") {
			t.Fatalf("stdout = %q, want capability confirmation", stdout.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
	})

	t.Run("restricted userns reports actionable diagnostic", func(t *testing.T) {
		deps := readyWSLDeps()
		deps.hostOS = "linux"
		deps.probeNetworkNone = func(context.Context) error {
			return fmt.Errorf("executor: network isolation probe: %w", syscall.EPERM)
		}
		var stdout, stderr bytes.Buffer
		if code := runOnboardingPreflightWith(nil, &stdout, &stderr, deps); code != 1 {
			t.Fatalf("code = %d, want 1, stdout = %q", code, stdout.String())
		}
		for _, want := range []string{"unprivileged user namespace", "GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE"} {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
			}
		}
	})

	t.Run("Windows-only flags rejected on Linux", func(t *testing.T) {
		deps := readyWSLDeps()
		deps.hostOS = "linux"
		var stdout, stderr bytes.Buffer
		if code := runOnboardingPreflightWith([]string{"--distro", "Ubuntu"}, &stdout, &stderr, deps); code != 2 {
			t.Fatalf("code = %d, want 2, stdout = %q", code, stdout.String())
		}
		if !strings.Contains(stderr.String(), "Windows-only") {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})
}
