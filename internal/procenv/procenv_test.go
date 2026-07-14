package procenv

import "testing"

// TestBaseEnvPassesThroughGoToolchainFamily is the regression test for #248:
// a stage's `local-ci` (`make ci` -> `go build`/`go test`) must see a
// relocated Go cache/module store/proxy, not silently fall back to
// HOME-derived defaults that don't exist on a customized host.
func TestBaseEnvPassesThroughGoToolchainFamily(t *testing.T) {
	goVars := map[string]string{
		"GOPATH":     "/custom/gopath",
		"GOBIN":      "/custom/gobin",
		"GOCACHE":    "/custom/gocache",
		"GOMODCACHE": "/custom/gomodcache",
		"GOFLAGS":    "-mod=mod",
		"GOPROXY":    "https://proxy.example.internal",
		"GOSUMDB":    "off",
		"GOPRIVATE":  "example.internal/*",
	}
	for name, value := range goVars {
		t.Setenv(name, value)
	}

	env := BaseEnv()
	for name, value := range goVars {
		want := name + "=" + value
		found := false
		for _, kv := range env {
			if kv == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s did not pass through BaseEnv(), got %v", name, env)
		}
	}
}

// TestBaseEnvStillBlocksSecretShapedVars proves the Go toolchain extension
// stays default-deny — only exact allowlisted names pass.
func TestBaseEnvStillBlocksSecretShapedVars(t *testing.T) {
	t.Setenv("GOPATH_TOKEN", "should-not-pass")
	t.Setenv("MY_GOPROXY_SECRET", "should-not-pass")
	t.Setenv("GOPATH", "/custom/gopath")

	env := BaseEnv()
	for _, kv := range env {
		if kv == "GOPATH_TOKEN=should-not-pass" || kv == "MY_GOPROXY_SECRET=should-not-pass" {
			t.Fatalf("blocked var leaked into BaseEnv(): %v", env)
		}
	}
}
