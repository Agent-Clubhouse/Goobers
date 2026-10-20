//go:build integration

package toolchain

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/mod/semver"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Exercise the actual resolver using the highest installed stable SDK's band.
// The unit runner test independently requires a checkout on every platform.
func TestIntegrationVerifyInHonorsDotnetGlobalJSON(t *testing.T) {
	testdep.Require(t, "dotnet")
	out, err := exec.Command("dotnet", "--list-sdks").CombinedOutput()
	if err != nil {
		t.Fatalf("dotnet --list-sdks: %v\n%s", err, out)
	}
	var installed string
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		version := "v" + fields[0]
		if semver.Canonical(version) != version || semver.Prerelease(version) != "" {
			continue
		}
		if installed == "" || semver.Compare(version, "v"+installed) > 0 {
			installed = fields[0]
		}
	}
	if installed == "" {
		t.Fatalf("dotnet --list-sdks listed no stable SDK:\n%s", out)
	}
	parts := strings.Split(installed, ".")
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		t.Fatalf("parse installed SDK %q: %v", installed, err)
	}
	band := fmt.Sprintf("%s.%s.%d", parts[0], parts[1], patch-patch%100)
	missing := fmt.Sprintf("%s.%s.%d", parts[0], parts[1], patch+1)
	required := "dotnet@" + parts[0]
	t.Logf("installed SDK %s; latestPatch pin %s; absent exact pin %s", installed, band, missing)
	for _, tc := range []struct {
		name, sdk, policy string
		wantError         bool
	}{
		{"roll forward to installed patch", band, "latestPatch", false},
		{"missing exact SDK fails closed", missing, "disable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pin := fmt.Sprintf(`{"sdk":{"version":%q,"rollForward":%q,"allowPrerelease":false}}`, tc.sdk, tc.policy)
			if err := os.WriteFile(filepath.Join(dir, "global.json"), []byte(pin), 0o600); err != nil {
				t.Fatal(err)
			}
			v := DefaultVerifier()
			err := v.VerifyIn(context.Background(), []string{required}, dir)
			if (err != nil) != tc.wantError {
				t.Fatalf("VerifyIn = %v, wantError %v", err, tc.wantError)
			}
			if !tc.wantError {
				cmd := exec.Command("dotnet", "--version")
				cmd.Dir = dir
				resolved, err := cmd.CombinedOutput()
				if err != nil || strings.TrimSpace(string(resolved)) != installed {
					t.Fatalf("latestPatch resolved %q, %v; want installed SDK %s", resolved, err, installed)
				}
			}
		})
	}
}
