package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Acquisitions supplement the executable allowlist: an allowed executable can
// still acquire code or data from outside the machine. Discover sites from the
// executable check list, never from the manifest we are trying to verify.
type acquisition struct {
	ID      string   `json:"id"`
	What    string   `json:"what"`
	From    []string `json:"from"`
	Offline string   `json:"offline"`
	Sites   []string `json:"sites"`
}

func checkAcquisitionSites(all []check) []string {
	var sites []string
	for _, c := range all {
		kind := checkAcquisitionKind(c)
		if kind != "" {
			sites = append(sites, c.label+":"+kind)
		}
		// A Go/npm invocation can also acquire data through the tool it
		// starts (schemas, advisories, browsers); module acquisition alone
		// does not account for those distinct origins and offline controls.
		sites = append(sites, scriptAcquisitionSites(c.label, c.command+" "+strings.Join(c.args, " "))...)
	}
	slices.Sort(sites)
	return slices.Compact(sites)
}

func TestAcquisitionDiscoverySeparatesToolDataFromModules(t *testing.T) {
	t.Parallel()
	sites := checkAcquisitionSites([]check{{label: "scan", command: "go", args: []string{"run", "golang.org/x/vuln/cmd/govulncheck@v1.6.0", "./..."}}})
	if !slices.Equal(sites, []string{"scan:go-modules", "scan:go-vulnerability-data"}) {
		t.Fatalf("tool data disappeared into module acquisition: %v", sites)
	}
}

func checkAcquisitionKind(c check) string {
	switch filepath.Base(c.command) {
	case "go":
		// Compilation, generation and module maintenance may all cause Go to
		// acquire modules or the toolchain selected by go.mod/GOTOOLCHAIN.
		return "go-modules"
	case "npm":
		if slices.Contains(c.args, "playwright") && slices.Contains(c.args, "install") {
			return "playwright-browsers"
		}
		if slices.Contains(c.args, "audit") {
			return "npm-advisories"
		}
		if slices.Contains(c.args, "ci") || slices.Contains(c.args, "install") || slices.Contains(c.args, "exec") {
			return "npm-packages"
		}
	}
	return ""
}

func acquisitionDrift(entries []acquisition, actual []string) error {
	if len(actual) == 0 {
		return fmt.Errorf("runtime acquisition discovery checked zero sites")
	}
	declared := make(map[string]bool)
	ids := make(map[string]bool)
	for _, entry := range entries {
		if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.What) == "" || len(entry.From) == 0 || strings.TrimSpace(entry.Offline) == "" || len(entry.Sites) == 0 {
			return fmt.Errorf("incomplete runtime acquisition %q", entry.ID)
		}
		if ids[entry.ID] {
			return fmt.Errorf("duplicate runtime acquisition ID %q", entry.ID)
		}
		ids[entry.ID] = true
		for _, origin := range entry.From {
			if strings.TrimSpace(origin) == "" {
				return fmt.Errorf("empty acquisition origin for %q", entry.ID)
			}
		}
		for _, site := range entry.Sites {
			if !strings.HasSuffix(site, ":"+entry.ID) {
				return fmt.Errorf("acquisition site %q does not belong to %q", site, entry.ID)
			}
			if declared[site] {
				return fmt.Errorf("duplicate runtime acquisition site %q", site)
			}
			declared[site] = true
		}
	}
	var missing []string
	for _, site := range actual {
		if !declared[site] {
			missing = append(missing, site)
		}
		delete(declared, site)
	}
	var stale []string
	for site := range declared {
		stale = append(stale, site)
	}
	slices.Sort(stale)
	if len(missing)+len(stale) > 0 {
		return fmt.Errorf("runtime acquisition manifest drift: undeclared=%q stale=%q", missing, stale)
	}
	return nil
}

func decodeAcquisitions(data []byte) ([]acquisition, error) {
	var entries []acquisition
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entries); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("runtime acquisition manifest must contain exactly one JSON value")
	}
	return entries, nil
}

func TestAcquisitionDecoderRejectsTrailingAndUnknownData(t *testing.T) {
	t.Parallel()
	for _, input := range []string{`[] {}`, `[] trailing`, `[{"unknown":true}]`} {
		if _, err := decodeAcquisitions([]byte(input)); err == nil {
			t.Errorf("accepted malformed manifest %q", input)
		}
	}
}

func TestAcquisitionManifestRefusesInvalidDeclarations(t *testing.T) {
	t.Parallel()
	entry := acquisition{ID: "npm-packages", What: "packages", From: []string{"registry"}, Offline: "local cache", Sites: []string{"install:npm-packages"}}
	for _, test := range []struct {
		name    string
		entries []acquisition
		actual  []string
	}{
		{"zero discovery", nil, nil},
		{"duplicate ID", []acquisition{entry, entry}, entry.Sites},
		{"stale site", []acquisition{entry}, []string{"different:npm-packages"}},
		{"wrong acquisition kind", []acquisition{entry}, []string{"install:playwright-browsers"}},
		{"missing override", []acquisition{{ID: entry.ID, What: entry.What, From: entry.From, Sites: entry.Sites}}, entry.Sites},
	} {
		t.Run(test.name, func(t *testing.T) {
			if acquisitionDrift(test.entries, test.actual) == nil {
				t.Fatal("invalid manifest passed")
			}
		})
	}
}

func TestRuntimeAcquisitionManifestCoversMergeChecks(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("runtime-acquisitions.json")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := decodeAcquisitions(data)
	if err != nil {
		t.Fatal(err)
	}
	tools := toolchain{goCommand: "go", gofmtCommand: "gofmt", gitCommand: "git", npmCommand: "npm", nodeCommand: "node", golangciCommand: "golangci-lint"}
	sdkSites, err := sdkAcquisitionSites(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	provisioning, err := provisioningAcquisitionSites(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	dependencies, err := dependencyAcquisitionSites(testdep.Dependencies())
	if err != nil {
		t.Fatal(err)
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		all := checks([]string{"config-sync", "goobers", "operator", "scheduler"}, tools, buildMetadata{}, goos, "test-timings/unit.json")
		actual := append(checkAcquisitionSites(all), sdkSites...)
		actual = append(actual, provisioning...)
		actual = append(actual, dependencies...)
		if err := acquisitionDrift(entries, actual); err != nil {
			t.Errorf("%s: %v", goos, err)
		}
	}
}

func TestNewAcquisitionIsDiscoveredWithoutManifestEntry(t *testing.T) {
	t.Parallel()
	// This adds an executable check, not a manifest fixture or scanner hint.
	// npm is already on the executable allowlist, so that guard cannot help.
	sites := checkAcquisitionSites([]check{{label: "new-tool", command: "npm", args: []string{"exec", "--", "new-tool"}}})
	if len(sites) != 1 || sites[0] != "new-tool:npm-packages" {
		t.Fatalf("new fetch was not discovered: %v", sites)
	}
	if err := acquisitionDrift(nil, sites); err == nil {
		t.Fatal("unregistered acquisition passed")
	}
}
