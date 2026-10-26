package dslmigrate

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestV21SurgicalPermissionMigration(t *testing.T) {
	for _, policy := range []string{
		"      policyActions: [label-issue] # preserve action note\n",
		"      policyActions:\n        - label-issue # preserve action note\n",
		"      policyActions: [] # empty explicit list\n",
		"",
	} {
		for _, eol := range []string{"\n", "\r\n"} {
			source := "# author heading\napiVersion: goobers.dev/v1alpha1\nkind: Workflow\ndslVersion: '2.0' # pin\nmetadata: {name: watcher, custom: untouched}\nspec:\n  tasks:\n    - name: watch\n      run: {command: [goobers, pr-comment-watch]}\n" + policy + "      goal: Keep this exact author text.\n    - name: unrelated\n      run: {command: [echo, pr-comment-watch]}\n"
			source = strings.ReplaceAll(source, "\n", eol)
			result, err := Migrate([]byte(source), "2.1")
			if err != nil {
				t.Fatal(err)
			}
			want := []PolicyAddition{{Task: "watch", Action: "comment-on-issue"}}
			if !reflect.DeepEqual(result.PolicyAdditions, want) {
				t.Fatalf("additions: %#v", result.PolicyAdditions)
			}
			if strings.Count(result.After, "comment-on-issue") != 1 {
				t.Fatal(result.After)
			}
			if strings.Contains(result.After, "goobers.dev/allow-preview-features") {
				t.Fatal("migration silently opted into preview features")
			}
			if !strings.Contains(strings.Join(result.Notes, " "), "Workflow.metadata.annotations") {
				t.Fatal("preview annotation ownership is unclear")
			}
			if !strings.Contains(result.After, "dslVersion: '2.1' # pin"+eol) || !strings.Contains(result.After, "metadata: {name: watcher, custom: untouched}"+eol) {
				t.Fatal(result.After)
			}
			again, err := Migrate([]byte(source), "2.1")
			if err != nil || !reflect.DeepEqual(result, again) {
				t.Fatalf("not deterministic: %v", err)
			}
			if _, err := Migrate([]byte(result.After), "2.1"); !errors.Is(err, ErrAlreadyAtTarget) {
				t.Fatalf("second migration: %v", err)
			}
		}
	}
}

func TestV21FlowTasksAndExistingAuthority(t *testing.T) {
	source := `dslVersion: "2.0"
spec: {tasks: [{name: one, run: {command: [goobers, pr-comment-watch]}, policyActions: [label-issue]}, {name: two, run: {command: [goobers, pr-comment-watch]}}, {name: authorized, run: {command: [goobers, pr-comment-watch]}, policyActions: [comment-on-issue, label-issue]}]}
`
	result, err := Migrate([]byte(source), "2.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PolicyAdditions) != 2 || strings.Count(result.After, "comment-on-issue") != 3 {
		t.Fatalf("%#v\n%s", result.PolicyAdditions, result.After)
	}
	if !strings.Contains(result.After, "{name: authorized, run: {command: [goobers, pr-comment-watch]}, policyActions: [comment-on-issue, label-issue]}") {
		t.Fatal("already-authorized task rewritten")
	}
}

func TestV21PinOnlyAndSeparateV3Path(t *testing.T) {
	for _, tasks := range []string{
		`[{name: allowed, run: {command: [goobers, pr-comment-watch]}, policyActions: [label-issue, comment-on-issue]}]`,
		`[{name: other, run: {command: [sh, -c, "goobers pr-comment-watch"]}}]`,
		`[{name: other, run: {command: [goobers, ci-poll]}}]`,
	} {
		source := "dslVersion: \"2.0\"\nspec: {tasks: " + tasks + "}\n"
		result, err := Migrate([]byte(source), "2.1")
		if err != nil {
			t.Fatal(err)
		}
		if len(result.PolicyAdditions) != 0 || result.After != strings.Replace(source, "2.0", "2.1", 1) {
			t.Fatal(result)
		}
	}
	if _, ok := FindEdge("2.0", "3.0"); !ok {
		t.Fatal("direct 2.0 to 3.0 path was removed")
	}
	if _, ok := FindEdge("2.1", "3.0"); ok {
		t.Fatal("2.1 must not imply an unimplemented 3.0 migration")
	}
}

func TestV21RefusesMalformedActionsWithoutRewriting(t *testing.T) {
	for _, actions := range []string{"null", "label-issue", "{label-issue: true}"} {
		source := "dslVersion: '2.0'\nspec: {tasks: [{name: watch, run: {command: [goobers, pr-comment-watch]}, policyActions: " + actions + "}]}\n"
		if _, err := Migrate([]byte(source), "2.1"); err == nil {
			t.Fatalf("accepted malformed actions %s", actions)
		}
	}
}
