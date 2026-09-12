package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func releaseServer(t *testing.T, latest string, all []map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			if latest == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": latest})
		case strings.HasSuffix(r.URL.Path, "/releases"):
			_ = json.NewEncoder(w).Encode(all)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCheckLatestChannels(t *testing.T) {
	// The prerelease channel must be able to select a target the stable
	// channel does not: that difference is the whole point of the setting.
	all := []map[string]any{
		{"tag_name": "v0.4.0"},
		{"tag_name": "v0.5.0-rc.1"},
	}
	server := releaseServer(t, "v0.4.0", all)

	tests := []struct {
		name       string
		channel    string
		current    string
		wantLatest string
		wantUpdate bool
	}{
		{name: "stable behind", channel: ChannelStable, current: "v0.3.0", wantLatest: "v0.4.0", wantUpdate: true},
		{name: "stable current", channel: ChannelStable, current: "v0.4.0", wantLatest: "v0.4.0"},
		{name: "stable ignores prerelease", channel: ChannelStable, current: "v0.4.0", wantLatest: "v0.4.0"},
		{name: "prerelease sees rc", channel: ChannelPrerelease, current: "v0.4.0", wantLatest: "v0.5.0-rc.1", wantUpdate: true},
		{name: "default channel is stable", channel: "", current: "v0.4.0", wantLatest: "v0.4.0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := CheckLatest(context.Background(), CheckOptions{
				CurrentVersion: test.current,
				Channel:        test.channel,
				Owner:          "acme",
				Repository:     "app",
				APIBaseURL:     server.URL,
				HTTPClient:     server.Client(),
			})
			if err != nil {
				t.Fatalf("CheckLatest() error = %v", err)
			}
			if result.LatestVersion != test.wantLatest {
				t.Errorf("LatestVersion = %q, want %q", result.LatestVersion, test.wantLatest)
			}
			if result.UpdateAvailable != test.wantUpdate {
				t.Errorf("UpdateAvailable = %t, want %t", result.UpdateAvailable, test.wantUpdate)
			}
		})
	}
}

// A development build carries Version "dev", which is not SemVer. Every plain
// `go build` produces one, so treating it as "behind" would make every
// developer's daemon announce an update on every start.
func TestCheckLatestDevBuildMakesNoRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v9.9.9"})
	}))
	defer server.Close()

	_, err := CheckLatest(context.Background(), CheckOptions{
		CurrentVersion: "dev",
		Owner:          "acme",
		Repository:     "app",
		APIBaseURL:     server.URL,
		HTTPClient:     server.Client(),
	})
	if !errors.Is(err, ErrVersionNotComparable) {
		t.Fatalf("CheckLatest() error = %v, want ErrVersionNotComparable", err)
	}
	if requests != 0 {
		t.Errorf("made %d requests for an incomparable build, want 0", requests)
	}
}

func TestCheckLatestRejectsUnknownChannel(t *testing.T) {
	_, err := CheckLatest(context.Background(), CheckOptions{CurrentVersion: "v1.0.0", Channel: "beta"})
	if err == nil || !strings.Contains(err.Error(), "unknown update-check channel") {
		t.Fatalf("CheckLatest() error = %v, want unknown channel", err)
	}
}

func TestCheckCacheRoundTrip(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadCheck(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadCheck() on empty root error = %v, want os.ErrNotExist", err)
	}
	want := CheckResult{CurrentVersion: "v0.4.0", LatestVersion: "v0.5.0", UpdateAvailable: true, Channel: ChannelStable}
	if err := WriteCheck(root, want); err != nil {
		t.Fatalf("WriteCheck() error = %v", err)
	}
	got, err := ReadCheck(root)
	if err != nil {
		t.Fatalf("ReadCheck() error = %v", err)
	}
	if got.LatestVersion != want.LatestVersion || !got.UpdateAvailable || got.Schema != checkSchema {
		t.Errorf("ReadCheck() = %+v, want %s/%s", got, want.LatestVersion, checkSchema)
	}
}

func TestReadCheckRejectsForeignSchema(t *testing.T) {
	root := t.TempDir()
	path := checkPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema":"other/v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCheck(root); err == nil || !strings.Contains(err.Error(), "unsupported update check schema") {
		t.Fatalf("ReadCheck() error = %v, want unsupported schema", err)
	}
}

// A notice that names `goobers self-update` on an unsupervised instance sends
// the operator into Prepare's hard refusal, so the call to action must branch
// on the supervised binary slot actually existing.
func TestNoticeCallToAction(t *testing.T) {
	available := CheckResult{CurrentVersion: "v0.4.0", LatestVersion: "v0.5.0", UpdateAvailable: true}
	if got := Notice(available, true); !strings.Contains(got, "goobers self-update") {
		t.Errorf("Notice(supervised) = %q, want the self-update command", got)
	}
	if got := Notice(available, false); !strings.Contains(got, "goobers service install") {
		t.Errorf("Notice(unsupervised) = %q, want the service install command", got)
	}
	if got := Notice(CheckResult{CurrentVersion: "v0.4.0", LatestVersion: "v0.4.0"}, true); got != "" {
		t.Errorf("Notice(up to date) = %q, want empty", got)
	}
}

func TestSupervised(t *testing.T) {
	root := t.TempDir()
	if Supervised(root) {
		t.Fatal("Supervised() = true for an instance with no binary slot")
	}
	binary := currentBinary(root, runtime.GOOS)
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !Supervised(root) {
		t.Error("Supervised() = false with the binary slot present")
	}
}
