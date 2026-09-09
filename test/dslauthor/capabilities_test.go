package dslauthor

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func checkFixtureCapabilities(scenario fixtureScenario, capabilities []string) error {
	// A fixture's real CI may use Ubuntu. Selecting a Linux worker is a
	// supported placement constraint, not additional repository authority.
	// Permission grants and explicit version pins remain exact. Unversioned
	// tools directly evidenced by the fixture are optional worker prerequisites.
	linuxCI := fixtureLinuxCI(scenario)
	tools := fixtureToolPrerequisites(scenario)
	actual := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		if (linuxCI && capability == "os=linux") || tools[capability] {
			continue
		}
		actual = append(actual, capability)
	}
	slices.Sort(actual)
	want := expectedCapabilities(scenario.Name)
	if !slices.Equal(actual, want) {
		return fmt.Errorf("got %v, want %v with only fixture-supported prerequisites", capabilities, want)
	}
	return nil
}

func fixtureLinuxCI(scenario fixtureScenario) bool {
	for path, content := range scenario.RepositoryFiles {
		if strings.HasPrefix(path, ".github/workflows/") && strings.Contains(content, "\n    runs-on: ubuntu-latest\n") {
			return true
		}
	}
	return strings.Contains(scenario.RepositoryFiles["azure-pipelines.yml"], "\n  vmImage: ubuntu-latest\n")
}

func fixtureToolPrerequisites(scenario fixtureScenario) map[string]bool {
	tools := map[string]bool{}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal([]byte(scenario.RepositoryFiles["package.json"]), &pkg) == nil && len(pkg.Scripts) > 0 {
		tools["node"], tools["npm"] = true, true
	}
	makefile := scenario.RepositoryFiles["Makefile"]
	if strings.TrimSpace(makefile) != "" {
		tools["make"] = true
	}
	// Never let an unversioned Go requirement replace an explicit go.mod
	// pin. The existing-config fixture invokes Go without declaring a pin.
	if scenario.RepositoryFiles["go.mod"] == "" && strings.Contains(makefile, "\tgo test ") {
		tools["go"] = true
	}
	return tools
}

func TestFixtureCapabilitiesPermitOnlyEvidenceBackedPlacement(t *testing.T) {
	scenario := fixtureScenario{Name: "node app", RepositoryFiles: map[string]string{
		".github/workflows/ci.yml": "jobs:\n  verify:\n    runs-on: ubuntu-latest\n",
	}}
	base := expectedCapabilities(scenario.Name)
	for _, extra := range []string{"", "os=linux", "os=windows", "repo:write", "github:pr:write", "go@1.26.5"} {
		got := slices.Clone(base)
		if extra != "" {
			got = append(got, extra)
		}
		err := checkFixtureCapabilities(scenario, got)
		if (err == nil) != (extra == "" || extra == "os=linux") {
			t.Fatalf("extra capability %q: %v", extra, err)
		}
	}
	if err := checkFixtureCapabilities(scenario, []string{"os=linux", "repo:read"}); err == nil {
		t.Fatal("placement substituted for missing authority")
	}
	scenario.RepositoryFiles[".github/workflows/ci.yml"] = "jobs:\n  verify:\n    runs-on: windows-latest\n"
	if err := checkFixtureCapabilities(scenario, append(slices.Clone(base), "os=linux")); err == nil {
		t.Fatal("unsupported placement was accepted without fixture evidence")
	}
}

func TestFixturePrerequisitesDoNotGrantAuthorityOrInventVersions(t *testing.T) {
	scenario := fixtureScenario{Name: "static documentation repo", RepositoryFiles: map[string]string{
		"package.json": `{"scripts":{"check:docs":"markdownlint-cli2 **/*.md"}}`,
	}}
	for _, capability := range []string{"node", "npm", "node@20", "npm@10", "repo:write", "github:issues:write", "make"} {
		err := checkFixtureCapabilities(scenario, []string{capability})
		if (err == nil) != (capability == "node" || capability == "npm") {
			t.Fatalf("capability %q: %v", capability, err)
		}
	}
	scenario = fixtureScenario{Name: "go service", RepositoryFiles: map[string]string{
		"go.mod": "module example\n\ngo 1.26.5\n", "Makefile": "ci:\n\tgo test ./...\n",
	}}
	if err := checkFixtureCapabilities(scenario, []string{"go@1.26.5", "make"}); err != nil {
		t.Fatal(err)
	}
	for _, got := range [][]string{{"go", "make"}, {"make"}, {"go@1.26.4", "make"}} {
		if err := checkFixtureCapabilities(scenario, got); err == nil {
			t.Fatalf("tool prerequisite hid missing or wrong version: %v", got)
		}
	}
}
