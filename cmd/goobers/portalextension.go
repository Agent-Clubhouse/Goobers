package main

import (
	"flag"
	"io"
	"strings"

	goobersassets "github.com/goobers/goobers"
	"github.com/goobers/goobers/internal/portalextension"
	"github.com/goobers/goobers/internal/version"
)

const portalExtensionHelp = "Usage: goobers portal-extension <subcommand> [flags]\n\n" +
	"Install, inspect, or update the user-scoped Goobers Portal canvas extension.\n" +
	"The extension is bundled with this binary, so its installed release identity\n" +
	"always matches the Goobers version supplying it.\n\n" +
	"Subcommands:\n" +
	"  install  install the bundled extension for the current user\n" +
	"  status   report installed version, drift, and updates\n" +
	"  update   replace a managed installation with this binary's version\n"

const portalExtensionInstallHelp = "Usage: goobers portal-extension install [--copilot-home <path>]\n\n" +
	"Install the Portal canvas extension beneath the current user's Copilot home.\n" +
	"Existing unmanaged, outdated, or locally modified directories are not replaced.\n"

const portalExtensionStatusHelp = "Usage: goobers portal-extension status [--copilot-home <path>]\n\n" +
	"Compare the installed Portal extension with the version bundled in this binary.\n" +
	"Exit codes: 0 = current, 1 = not installed, outdated, modified, or unmanaged.\n"

const portalExtensionUpdateHelp = "Usage: goobers portal-extension update [--replace-modified] [--copilot-home <path>]\n\n" +
	"Update a managed Portal extension through a staged, crash-recoverable replacement\n" +
	"binary. Local changes require --replace-modified. Persisted Portal sources and\n" +
	"preferences live outside the code directory and are preserved.\n"

var (
	detectCopilotAppForInit = portalextension.DetectCopilotApp
	installPortalForInit    = installCurrentPortalExtension
)

func runPortalExtension(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		pf(stdout, "%s", portalExtensionHelp)
		return 0
	}
	if len(args) > 0 {
		pf(stderr, "error: unknown portal-extension command %q\n", args[0])
	}
	pf(stderr, "%s", portalExtensionHelp)
	return 2
}

func runPortalExtensionInstall(args []string, stdout, stderr io.Writer) int {
	manager, bundle, code := portalExtensionInputs("portal-extension install", args, stderr, nil)
	if code != 0 {
		return code
	}
	result, err := manager.Install(bundle)
	if err != nil {
		pf(stderr, "error: install Portal extension: %v\n", err)
		return 1
	}
	if result.Installed {
		pf(stdout, "Installed Goobers Portal %s at %s.\n", bundle.Manifest.Version, result.Path)
	} else {
		pf(stdout, "Goobers Portal %s is already current at %s.\n", bundle.Manifest.Version, result.Path)
	}
	pf(stdout, "Reload extensions in the Copilot app to activate it.\n")
	return 0
}

func runPortalExtensionStatus(args []string, stdout, stderr io.Writer) int {
	manager, bundle, code := portalExtensionInputs("portal-extension status", args, stderr, nil)
	if code != 0 {
		return code
	}
	report, err := manager.Check(bundle)
	if err != nil {
		pf(stderr, "error: inspect Portal extension: %v\n", err)
		return 1
	}
	writePortalExtensionReport(stdout, report)
	if report.State != "current" {
		return 1
	}
	return 0
}

func runPortalExtensionUpdate(args []string, stdout, stderr io.Writer) int {
	replaceModified := false
	manager, bundle, code := portalExtensionInputs("portal-extension update", args, stderr, &replaceModified)
	if code != 0 {
		return code
	}
	result, err := manager.Update(bundle, replaceModified)
	if err != nil {
		pf(stderr, "error: update Portal extension: %v\n", err)
		return 1
	}
	if result.Installed {
		pf(stdout, "Updated Goobers Portal to %s at %s.\n", bundle.Manifest.Version, result.Path)
		pf(stdout, "Reload extensions in the Copilot app to activate it.\n")
	} else {
		pf(stdout, "Goobers Portal %s is already current at %s.\n", bundle.Manifest.Version, result.Path)
	}
	return 0
}

func portalExtensionInputs(command string, args []string, stderr io.Writer, replaceModified *bool) (*portalextension.Manager, portalextension.Bundle, int) {
	fs := newCLIFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	copilotHome := fs.String("copilot-home", "", "override the current user's Copilot home")
	if replaceModified != nil {
		fs.BoolVar(replaceModified, "replace-modified", false, "replace locally modified extension-owned files")
	}
	fs.Usage = helpUsage(stderr, command)
	if err := fs.Parse(args); err != nil {
		return nil, portalextension.Bundle{}, 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return nil, portalextension.Bundle{}, 2
	}
	home := strings.TrimSpace(*copilotHome)
	if home == "" {
		var err error
		home, err = portalextension.DefaultCopilotHome()
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return nil, portalextension.Bundle{}, 1
		}
	}
	manager, err := portalextension.Open(home)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return nil, portalextension.Bundle{}, 1
	}
	bundle, err := currentPortalExtensionBundle()
	if err != nil {
		pf(stderr, "error: build bundled Portal extension: %v\n", err)
		return nil, portalextension.Bundle{}, 1
	}
	return manager, bundle, 0
}

func currentPortalExtensionBundle() (portalextension.Bundle, error) {
	info := version.Get()
	return portalextension.Build(goobersassets.PortalExtensionAssets, info.Version, info.Commit)
}

func installCurrentPortalExtension() (portalextension.InstallResult, error) {
	home, err := portalextension.DefaultCopilotHome()
	if err != nil {
		return portalextension.InstallResult{}, err
	}
	manager, err := portalextension.Open(home)
	if err != nil {
		return portalextension.InstallResult{}, err
	}
	bundle, err := currentPortalExtensionBundle()
	if err != nil {
		return portalextension.InstallResult{}, err
	}
	return manager.Install(bundle)
}

func writePortalExtensionReport(w io.Writer, report portalextension.Report) {
	pf(w, "state: %s\n", report.State)
	pf(w, "path: %s\n", report.Path)
	pf(w, "bundled Goobers version: %s\n", report.SourceVersion)
	pf(w, "bundled Goobers commit: %s\n", report.SourceCommit)
	if report.InstalledVersion == "" {
		pf(w, "installed Goobers version: none\n")
		pf(w, "installed Goobers commit: none\n")
	} else {
		pf(w, "installed Goobers version: %s\n", report.InstalledVersion)
		pf(w, "installed Goobers commit: %s\n", report.InstalledCommit)
	}
	writeAgentKitPathList(w, "modified files", report.Modified)
	writeAgentKitPathList(w, "missing files", report.Missing)
	writeAgentKitPathList(w, "unexpected files", report.Unexpected)
}

func offerGuidedPortalExtension(p guidedPrompter) error {
	if !detectCopilotAppForInit() {
		return nil
	}
	pln(p.out, "")
	pln(p.out, "GitHub Copilot app detected.")
	answer, err := p.ask("Install the release-matched Goobers Portal canvas extension? (yes/no)", "yes", validYesNo)
	if err != nil {
		return err
	}
	if !isYes(answer) {
		pln(p.out, "Portal installation declined. Install it later with: goobers portal-extension install")
		return nil
	}
	result, err := installPortalForInit()
	if err != nil {
		pf(p.out, "Portal extension installation was skipped: %v\n", err)
		pln(p.out, "Install or update it later with: goobers portal-extension install")
		pln(p.out, "For an existing managed installation, run: goobers portal-extension update")
		return nil
	}
	if result.Installed {
		pf(p.out, "Installed Goobers Portal at %s. Reload extensions in the Copilot app to activate it.\n", result.Path)
	} else {
		pf(p.out, "Goobers Portal is already current at %s.\n", result.Path)
	}
	return nil
}
