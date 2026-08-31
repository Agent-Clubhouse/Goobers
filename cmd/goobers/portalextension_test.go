package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/portalextension"
	"github.com/goobers/goobers/internal/version"
)

func TestPortalExtensionCLIInstallStatusAndUpdate(t *testing.T) {
	home := t.TempDir()
	originalVersion, originalCommit := version.Version, version.Commit
	t.Cleanup(func() {
		version.Version = originalVersion
		version.Commit = originalCommit
	})
	version.Version, version.Commit = "v1.0.0", "old123"

	code, stdout, stderr := runArgs(t, "portal-extension", "install", "--copilot-home", home)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "Installed Goobers Portal v1.0.0") {
		t.Fatalf("install: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = runArgs(t, "portal-extension", "status", "--copilot-home", home)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "state: current") {
		t.Fatalf("status: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	version.Version, version.Commit = "v1.1.0", "new456"
	code, stdout, stderr = runArgs(t, "portal-extension", "status", "--copilot-home", home)
	if code != 1 || stderr != "" || !strings.Contains(stdout, "state: update-available") {
		t.Fatalf("outdated status: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = runArgs(t, "portal-extension", "update", "--copilot-home", home)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "Updated Goobers Portal to v1.1.0") {
		t.Fatalf("update: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestGuidedPortalInstallRequiresCopilotApp(t *testing.T) {
	originalDetect, originalInstall := detectCopilotAppForInit, installPortalForInit
	t.Cleanup(func() {
		detectCopilotAppForInit = originalDetect
		installPortalForInit = originalInstall
	})
	detectCopilotAppForInit = func() bool { return false }
	server := newTestGuidedServer(t, t.TempDir())
	response := guidedPost(http.HandlerFunc(server.serveGuided), "/guided/actions/install-portal-extension", "{}")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "copilot_app_unavailable") {
		t.Fatalf("install without app: status=%d body=%q", response.Code, response.Body.String())
	}

	detectCopilotAppForInit = func() bool { return true }
	installed := false
	installPortalForInit = func() (portalextension.InstallResult, error) {
		installed = true
		return portalextension.InstallResult{Path: "test-path", Installed: true}, nil
	}
	response = guidedPost(http.HandlerFunc(server.serveGuided), "/guided/actions/install-portal-extension", "{}")
	if response.Code != http.StatusOK || !installed || !strings.Contains(response.Body.String(), `"path":"test-path"`) {
		t.Fatalf("install: installed=%t status=%d body=%q", installed, response.Code, response.Body.String())
	}
}

func TestGuidedPortalInstallFailureIsNonFatal(t *testing.T) {
	originalDetect, originalInstall := detectCopilotAppForInit, installPortalForInit
	t.Cleanup(func() {
		detectCopilotAppForInit = originalDetect
		installPortalForInit = originalInstall
	})
	detectCopilotAppForInit = func() bool { return true }
	installPortalForInit = func() (portalextension.InstallResult, error) {
		return portalextension.InstallResult{}, errors.New("existing extension needs an update")
	}
	server := newTestGuidedServer(t, t.TempDir())
	response := guidedPost(http.HandlerFunc(server.serveGuided), "/guided/actions/install-portal-extension", "{}")
	output := response.Body.String()
	if response.Code != http.StatusConflict {
		t.Fatalf("install failure status=%d body=%q", response.Code, output)
	}
	for _, want := range []string{"existing extension needs an update", "portal-extension install", "portal-extension update"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output %q missing %q", output, want)
		}
	}
}
