package validate

import (
	"strings"
	"testing"
)

func TestArtifactManifestDeclarationAdmission(t *testing.T) {
	for name, tail := range map[string]string{
		"both":             "      inputs:\n        artifactManifestFile: manifest.json\n        artifactFile: legacy.txt\n",
		"blank":            "      inputs:\n        artifactManifestFile: ''\n",
		"escape":           "      inputs:\n        artifactManifestFile: ../manifest.json\n",
		"dynamic conflict": "      inputs:\n        artifactFile: legacy.txt\n      inputsFrom:\n        artifactManifestFile: draft.outputPath\n",
	} {
		t.Run(name, func(t *testing.T) {
			report := validateCompilerAdmission(t, tail)
			got := joinIssues(report)
			if !strings.Contains(got, "WF012") || !strings.Contains(got, "artifactManifestFile") {
				t.Fatalf("missing artifact declaration rejection:\n%s", got)
			}
		})
	}
	for _, tail := range []string{
		"      inputs:\n        artifactManifestFile: .goobers/manifest.json\n",
		"      inputs:\n        artifactFile: legacy.txt\n",
	} {
		got := joinIssues(validateCompilerAdmission(t, tail))
		if strings.Contains(got, "WF012") {
			t.Fatalf("valid declaration rejected:\n%s", got)
		}
	}
}
