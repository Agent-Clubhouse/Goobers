//go:build integration

package toolchain

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Exercise the actual SDK resolver when the regression's SDK family is present.
// The unit runner test independently requires a checkout on every platform.
func TestIntegrationVerifyInHonorsDotnetGlobalJSON(t *testing.T) {
	testdep.Require(t, "dotnet")
	out, err := exec.Command("dotnet", "--list-sdks").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "8.0.4") {
		t.Skip("requires an installed .NET 8.0.4xx SDK")
	}
	for _, tc := range []struct {
		name, sdk, policy string
		wantError         bool
	}{
		{"roll forward to installed patch", "8.0.400", "latestPatch", false},
		{"missing exact SDK fails closed", "8.0.499", "disable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pin := fmt.Sprintf(`{"sdk":{"version":%q,"rollForward":%q}}`, tc.sdk, tc.policy)
			if err := os.WriteFile(filepath.Join(dir, "global.json"), []byte(pin), 0o600); err != nil {
				t.Fatal(err)
			}
			v := DefaultVerifier()
			err := v.VerifyIn(context.Background(), []string{"dotnet@8"}, dir)
			if (err != nil) != tc.wantError {
				t.Fatalf("VerifyIn = %v, wantError %v", err, tc.wantError)
			}
		})
	}
}
