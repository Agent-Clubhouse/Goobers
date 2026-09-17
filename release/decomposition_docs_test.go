package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped decomposition publisher is intentionally repository-local. Keep
// that authoring boundary discoverable from both the design and the multi-repo
// onboarding path so configuration examples cannot imply unsupported routing.
func TestDecompositionRepositoryBoundaryDocumentation(t *testing.T) {
	design := compactDocumentation(readRepositoryDocumentation(t, "docs", "design", "decomposition-workflow.md"))
	for _, want := range []string{
		"### Repository boundary",
		"configured repository",
		"no repository or target-gaggle selector",
		"repository-qualified IDs are not a supported plan syntax",
		"`additionalRepos` supplies declared reference content",
		"separate repo-owning implementation workflows",
		"../guides/arbitrary-repo-onboarding.md#scale-out-with-more-gaggles-and-repositories",
	} {
		if !strings.Contains(design, want) {
			t.Errorf("decomposition design missing %q", want)
		}
	}

	onboarding := compactDocumentation(readRepositoryDocumentation(t, "docs", "guides", "arbitrary-repo-onboarding.md"))
	for _, want := range []string{
		"does not make one decomposition workflow a cross-repository publisher",
		"../design/decomposition-workflow.md#repository-boundary",
		"An `additionalRepos` declaration remains read-only reference access",
	} {
		if !strings.Contains(onboarding, want) {
			t.Errorf("arbitrary-repo onboarding guide missing %q", want)
		}
	}
}

func compactDocumentation(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func readRepositoryDocumentation(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{".."}, parts...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
