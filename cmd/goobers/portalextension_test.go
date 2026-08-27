package main

import (
	"bufio"
	"bytes"
	"errors"
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

func TestGuidedPortalOfferOnlyWhenCopilotAppDetected(t *testing.T) {
	originalDetect, originalInstall := detectCopilotAppForInit, installPortalForInit
	t.Cleanup(func() {
		detectCopilotAppForInit = originalDetect
		installPortalForInit = originalInstall
	})
	detectCopilotAppForInit = func() bool { return false }
	var output bytes.Buffer
	if err := offerGuidedPortalExtension(guidedPrompter{reader: bufio.NewReader(strings.NewReader("")), out: &output}); err != nil {
		t.Fatal(err)
	}

	if output.Len() != 0 {
		t.Fatalf("offer output without app detection = %q", output.String())
	}

	detectCopilotAppForInit = func() bool { return true }
	installed := false
	installPortalForInit = func() (portalextension.InstallResult, error) {
		installed = true
		return portalextension.InstallResult{Path: "test-path", Installed: true}, nil
	}
	output.Reset()
	if err := offerGuidedPortalExtension(guidedPrompter{reader: bufio.NewReader(strings.NewReader("no\n")), out: &output}); err != nil {
		t.Fatal(err)
	}
	if installed || !strings.Contains(output.String(), "goobers portal-extension install") {
		t.Fatalf("decline installed=%t output=%q", installed, output.String())
	}

	output.Reset()
	if err := offerGuidedPortalExtension(guidedPrompter{reader: bufio.NewReader(strings.NewReader("\n")), out: &output}); err != nil {
		t.Fatal(err)
	}
	if !installed || !strings.Contains(output.String(), "Reload extensions") {
		t.Fatalf("accept installed=%t output=%q", installed, output.String())
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
	var output bytes.Buffer
	err := offerGuidedPortalExtension(guidedPrompter{
		reader: bufio.NewReader(strings.NewReader("yes\n")),
		out:    &output,
	})
	if err != nil {
		t.Fatalf("optional Portal failure aborted setup: %v", err)
	}
	for _, want := range []string{"existing extension needs an update", "portal-extension install", "portal-extension update"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output %q missing %q", output.String(), want)
		}
	}
}
