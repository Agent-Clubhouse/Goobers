package version

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func TestRevertedJournalOTLPCapability(t *testing.T) {
	data, err := json.Marshal(Get())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"capabilities":[]`) {
		t.Fatalf("advertises unsupported capabilities: %s", data)
	}
}

func TestGetPopulatesRuntime(t *testing.T) {
	got := Get()
	if got.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", got.GoVersion, runtime.Version())
	}
	wantPlatform := runtime.GOOS + "/" + runtime.GOARCH
	if got.Platform != wantPlatform {
		t.Errorf("Platform = %q, want %q", got.Platform, wantPlatform)
	}
}

func TestInfoStringIncludesFields(t *testing.T) {
	i := Info{Version: "v1.2.3", Commit: "abc123", Date: "2026-06-28", GoVersion: "go1.23", Platform: "linux/amd64"}
	s := i.String()
	for _, want := range []string{"v1.2.3", "abc123", "2026-06-28", "go1.23", "linux/amd64"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
}
