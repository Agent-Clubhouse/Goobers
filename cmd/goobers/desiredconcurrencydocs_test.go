package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"

	"sigs.k8s.io/yaml"
)

const desiredConcurrencyGuide = "docs/guides/desired-concurrency-and-pause.md"

var guideExampleBlock = regexp.MustCompile("(?s)<!-- example: ([a-z-]+) -->\\s*```yaml\\n(.*?)```")

func desiredConcurrencyGuideExamples(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(desiredConcurrencyGuide)))
	if err != nil {
		t.Fatalf("read guide: %v", err)
	}
	examples := map[string]string{}
	for _, match := range guideExampleBlock.FindAllStringSubmatch(strings.ReplaceAll(string(raw), "\r\n", "\n"), -1) {
		examples[match[1]] = match[2]
	}
	for _, name := range []string{"refill", "paused-workflow", "paused-gaggle"} {
		if examples[name] == "" {
			t.Fatalf("guide is missing the %q example", name)
		}
	}
	return examples
}

func validateGuideConfig(t *testing.T, config string) *validate.Report {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	report, err := v.ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	return report
}

func guideDocument(t *testing.T, config, kind string) string {
	t.Helper()
	for _, doc := range strings.Split(config, "---\n") {
		if strings.Contains(doc, "\nkind: "+kind+"\n") {
			return doc
		}
	}
	t.Fatalf("example has no %s document", kind)
	return ""
}

func TestDesiredConcurrencyGuideExamples(t *testing.T) {
	examples := desiredConcurrencyGuideExamples(t)
	refill := examples["refill"]

	if report := validateGuideConfig(t, refill); report.HasErrors() {
		t.Fatalf("refill example does not validate: %+v", report.Issues)
	}
	var wf apiv1.Workflow
	if err := yaml.Unmarshal([]byte(guideDocument(t, refill, "Workflow")), &wf); err != nil {
		t.Fatal(err)
	}
	var gaggle apiv1.Gaggle
	if err := yaml.Unmarshal([]byte(guideDocument(t, refill, "Gaggle")), &gaggle); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "example", Name: "acme"}}}
	repoRef := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "acme"}
	counter, err := buildRefillDemandCounter(cfg, gaggle, &wf, repoRef, nil, nil, "", "", nil)
	if err != nil || counter == nil {
		t.Fatalf("refill example must wire a refill counter, got %v, %v", counter, err)
	}

	t.Run("desired zero is rejected", func(t *testing.T) {
		zero := strings.Replace(refill, "desiredConcurrentRuns: 2", "desiredConcurrentRuns: 0", 1)
		if zero == refill {
			t.Fatal("refill example no longer sets desiredConcurrentRuns: 2")
		}
		if report := validateGuideConfig(t, zero); !report.HasErrors() {
			t.Fatal("desiredConcurrentRuns: 0 validated; the guide says it is rejected")
		}
		omitted := strings.Replace(refill, "    desiredConcurrentRuns: 2\n", "", 1)
		if report := validateGuideConfig(t, omitted); report.HasErrors() {
			t.Fatalf("omitting desiredConcurrentRuns must validate: %+v", report.Issues)
		}
	})

	for _, tc := range []struct{ example, kind string }{
		{"paused-workflow", "Workflow"},
		{"paused-gaggle", "Gaggle"},
	} {
		t.Run(tc.example, func(t *testing.T) {
			paused := examples[tc.example]
			original := guideDocument(t, refill, tc.kind)
			if strings.Replace(paused, "  enabled: false\n", "", 1) != original {
				t.Fatalf("%s must be the refill example's %s plus enabled: false", tc.example, tc.kind)
			}
			config := strings.Replace(refill, original, paused, 1)
			if report := validateGuideConfig(t, config); report.HasErrors() {
				t.Fatalf("%s does not validate: %+v", tc.example, report.Issues)
			}
			var pausedWF apiv1.Workflow
			if err := yaml.Unmarshal([]byte(guideDocument(t, config, "Workflow")), &pausedWF); err != nil {
				t.Fatal(err)
			}
			var pausedGaggle apiv1.Gaggle
			if err := yaml.Unmarshal([]byte(guideDocument(t, config, "Gaggle")), &pausedGaggle); err != nil {
				t.Fatal(err)
			}
			if resolveDisabledReason(pausedGaggle, &pausedWF) == "" {
				t.Fatalf("%s does not disable the workflow", tc.example)
			}
		})
	}
}

func TestDesiredConcurrencyGuideIsLinkedFromCLIHelp(t *testing.T) {
	for name, help := range map[string]string{
		"status":     statusHelp,
		"run cancel": runCancelHelp,
		"down":       downHelp,
	} {
		if !strings.Contains(help, desiredConcurrencyGuide) {
			t.Errorf("%s help does not link %s", name, desiredConcurrencyGuide)
		}
	}
}
