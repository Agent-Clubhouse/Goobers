package dslmigrate

import (
	"strings"
	"testing"
)

func TestMigrateRefusesAdditionalDocumentsBeforeRewriting(t *testing.T) {
	for _, migration := range []struct{ source, target string }{
		{workflowWithUnpinnedCIPoll, "2.0"},
		{workflowWithPinnedCIPoll, "2.0"},
		{strings.Replace(workflowWithUnpinnedCIPoll, `dslVersion: "1.4"`, `dslVersion: "2.0"`, 1), "3.0"},
	} {
		for _, trailing := range []string{
			"---\nkind: Gaggle\nmetadata:\n  name: keep-me\n",
			"---\n",
			"---\ninvalid: [\n",
		} {
			result, err := Migrate([]byte(migration.source+trailing), migration.target)
			if err == nil || result != nil {
				t.Fatalf("Migrate returned output for a multi-document file: result=%+v err=%v", result, err)
			}
		}
	}
}

func TestMigrateRefusesAmbiguousYAMLBeforeRewriting(t *testing.T) {
	for _, tc := range []struct{ name, source, want string }{
		{"duplicate version", "kind: Workflow\ndslVersion: '1.4'\ndslVersion: '2.0'\n", "duplicate mapping key"},
		{"duplicate nested field", strings.Replace(workflowWithUnpinnedCIPoll, "  gaggle: golden", "  gaggle: golden\n  gaggle: other", 1), "duplicate mapping key"},
		{"alias", "kind: Workflow\ndslVersion: '1.4'\nmetadata: &meta {name: workflow}\nspec: *meta\n", "YAML aliases"},
		{"inline merge", "kind: Workflow\ndslVersion: '1.4'\nspec:\n  <<: {gaggle: inherited}\n", "YAML merge keys"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Migrate([]byte(tc.source), "2.0")
			if err == nil || !strings.Contains(err.Error(), tc.want) || result != nil {
				t.Fatalf("Migrate = %+v, %v; want no output and %q", result, err, tc.want)
			}
		})
	}
}
