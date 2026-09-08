package main

import (
	"fmt"
	"slices"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

type dependencyAcquisition struct {
	kind   string
	reason string
}

// Every addition to the executable needs-list requires an explicit acquisition
// decision. Presence on PATH is not sufficient for package-manager drivers.
// Empty kind means no lazy dependency acquisition in the shipped fixture, not
// a claim that arbitrary programs run through this executable are offline.
var dependencyAcquisitionDecisions = map[string]dependencyAcquisition{
	"bash":           {reason: "interpreter; fixture command acquisitions are inventoried separately"},
	"bwrap":          {reason: "preinstalled containment executable"},
	"copilot":        {reason: "preinstalled CLI; live service tests are separately opt-in, not binary provisioning"},
	"cp":             {reason: "local coreutils operation"},
	"dirname":        {reason: "local coreutils operation"},
	"dotnet":         {kind: "nuget-packages", reason: "opt-in dotnet-service fixture executes dotnet test with restore"},
	"find":           {reason: "local findutils operation"},
	"git":            {reason: "integration fixtures operate on locally seeded repositories"},
	"head":           {reason: "local coreutils operation"},
	"java":           {reason: "preinstalled JVM; Maven resolves fixture dependencies"},
	"mkdir":          {reason: "local coreutils operation"},
	"mvn":            {kind: "maven-packages", reason: "Java fixture executes Maven verify"},
	"powershell.exe": {reason: "preinstalled Windows PowerShell 5.1; Windows-only image fixtures use local files and offline metadata"},
	"ps":             {reason: "local process inspection"},
	"python3":        {reason: "shipped Python fixture uses the standard library, not pip"},
	"sh":             {reason: "interpreter; fixture command acquisitions are inventoried separately"},
	"sleep":          {reason: "local coreutils operation"},
	"yes":            {reason: "local coreutils operation"},
	"xcodebuild":     {reason: "opt-in fixture requires preinstalled Xcode and simulator SDK"},
	"xcrun":          {reason: "opt-in fixture selects an already available simulator runtime"},
}

func dependencyAcquisitionSites(dependencies []testdep.Dependency) ([]string, error) {
	seen := make(map[string]bool)
	var sites []string
	for _, dependency := range dependencies {
		decision, ok := dependencyAcquisitionDecisions[dependency.Name]
		if !ok || decision.reason == "" {
			return nil, fmt.Errorf("integration executable %q has no runtime acquisition decision", dependency.Name)
		}
		seen[dependency.Name] = true
		if decision.kind != "" {
			sites = append(sites, "integration-dependency/"+dependency.Name+":"+decision.kind)
		}
	}
	for name := range dependencyAcquisitionDecisions {
		if !seen[name] {
			return nil, fmt.Errorf("stale acquisition decision for removed integration executable %q", name)
		}
	}
	slices.Sort(sites)
	return sites, nil
}

func TestNewAcquisitionDependencyNeedsExplicitDecision(t *testing.T) {
	t.Parallel()
	dependencies := append(testdep.Dependencies(), testdep.Dependency{Name: "new-package-manager"})
	if _, err := dependencyAcquisitionSites(dependencies); err == nil {
		t.Fatal("new executable passed without an acquisition decision")
	}
}
