package gaggletemplate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testRevision = "1111111111111111111111111111111111111111"

const testGaggle = `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: example
spec:
  project: {provider: github, owner: acme, name: app}
  backlog: {provider: github, project: acme/app}
  isolation: {namespace: example}
`

func treeFile(text string) File { return File{Data: []byte(text), Mode: 0644} }

func testSource() Source {
	return Source{SchemaVersion: 1, Repository: "local-repo", Ref: "main", Directory: "templates/example", Gaggle: "orders"}
}

func TestMergeDisjointYAMLAndText(t *testing.T) {
	base := Tree{
		"workflow.yaml":   treeFile("spec:\n  readiness: {maxConcurrentRuns: 1}\n  tasks:\n    - name: review\n      goal: old\n"),
		"instructions.md": treeFile("old instructions\n"),
	}
	local := Tree{
		"workflow.yaml":   treeFile("spec:\n  readiness: {maxConcurrentRuns: 2}\n  tasks:\n    - name: review\n      goal: old\n"),
		"instructions.md": treeFile("old instructions\n"),
	}
	upstream := Tree{
		"workflow.yaml":   treeFile("spec:\n  readiness: {maxConcurrentRuns: 1}\n  tasks:\n    - name: review\n      goal: improved\n"),
		"instructions.md": treeFile("better instructions\n"),
	}
	merged, conflicts, err := Merge(base, local, upstream)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("merge: conflicts=%v err=%v", conflicts, err)
	}
	for _, want := range []string{"maxConcurrentRuns: 2", "goal: improved"} {
		if !strings.Contains(string(merged["workflow.yaml"].Data), want) {
			t.Fatalf("missing %q in %s", want, merged["workflow.yaml"].Data)
		}
	}
	if string(merged["instructions.md"].Data) != "better instructions\n" {
		t.Fatal("upstream-only instruction edit lost")
	}
	if !containsEdits(base, local, merged) {
		t.Fatal("source containing merged runtime edits should be deployable")
	}
}

func TestMergeConflictsDoNotChooseAWinner(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		base, local, upstream Tree
	}{
		{"same-field", Tree{"a.yaml": treeFile("value: 1\n")}, Tree{"a.yaml": treeFile("value: 2\n")}, Tree{"a.yaml": treeFile("value: 3\n")}},
		{"delete-edit", Tree{"a.md": treeFile("base")}, Tree{"a.md": treeFile("mine")}, Tree{}},
		{"add-add", Tree{}, Tree{"a.md": treeFile("mine")}, Tree{"a.md": treeFile("theirs")}},
		{"text", Tree{"a.md": treeFile("base")}, Tree{"a.md": treeFile("mine")}, Tree{"a.md": treeFile("theirs")}},
		{"ordered-list", Tree{"a.yaml": treeFile("items: [a, b]\n")}, Tree{"a.yaml": treeFile("items: [b, a]\n")}, Tree{"a.yaml": treeFile("items: [a, b, c]\n")}},
		{"null-false", Tree{"a.yaml": treeFile("enabled: true\n")}, Tree{"a.yaml": treeFile("enabled: false\n")}, Tree{"a.yaml": treeFile("enabled: null\n")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.local.Digest()
			_, conflicts, err := Merge(tc.base, tc.local, tc.upstream)
			if err != nil || len(conflicts) != 1 {
				t.Fatalf("conflicts=%v error=%v", conflicts, err)
			}
			if tc.local.Digest() != before {
				t.Fatal("merge mutated original files")
			}
		})
	}
}

func TestMergePreservesFalseZeroNullAndDeletion(t *testing.T) {
	base := Tree{"a.yaml": treeFile("enabled: true\nlimit: 1\nnullable: x\nremove: x\nother: old\n")}
	local := Tree{"a.yaml": treeFile("enabled: false\nlimit: 0\nnullable: null\nother: old\n")}
	upstream := Tree{"a.yaml": treeFile("enabled: true\nlimit: 1\nnullable: x\nremove: x\nother: new\n")}
	result, conflicts, err := Merge(base, local, upstream)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("%v %v", conflicts, err)
	}
	want := Tree{"a.yaml": treeFile("enabled: false\nlimit: 0\nnullable: null\nother: new\n")}
	if !Equivalent(result, want) {
		t.Fatalf("wrong result %s", result["a.yaml"].Data)
	}
}

func TestMergeTaskReorderConflictsWithConcurrentEdit(t *testing.T) {
	base := Tree{"workflow.yaml": treeFile("spec:\n  tasks: [{name: a, goal: old}, {name: b}]\n")}
	local := Tree{"workflow.yaml": treeFile("spec:\n  tasks: [{name: b}, {name: a, goal: old}]\n")}
	upstream := Tree{"workflow.yaml": treeFile("spec:\n  tasks: [{name: a, goal: new}, {name: b}]\n")}
	if _, conflicts, err := Merge(base, local, upstream); err != nil || len(conflicts) != 1 {
		t.Fatalf("reordering silently lost: %v %v", conflicts, err)
	}
}

func TestBindNamesAndSafety(t *testing.T) {
	base := Tree{
		"gaggle.yaml":                      treeFile(testGaggle),
		"goobers/reviewer/goober.yaml":     treeFile("kind: Goober\nmetadata: {name: reviewer}\nspec: {gaggle: example, instructions: instructions.md}\n"),
		"goobers/reviewer/instructions.md": treeFile("reviewer example: do not rewrite this prose"),
		"workflows/work.yaml":              treeFile("kind: Workflow\nmetadata: {name: work}\nspec:\n  gaggle: example\n  tasks:\n    - name: review\n      goober: reviewer\n"),
	}
	first, err := Bind(base, "orders")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Bind(base, "orders")
	if err != nil || first.Digest() != second.Digest() {
		t.Fatal("binding is not deterministic")
	}
	if !strings.Contains(string(first["gaggle.yaml"].Data), "enabled: false") ||
		!strings.Contains(string(first["workflows/work.yaml"].Data), "goober: orders-reviewer") {
		t.Fatal("missing safety disable or typed reference rewrite")
	}
	if string(first["goobers/reviewer/instructions.md"].Data) != string(base["goobers/reviewer/instructions.md"].Data) {
		t.Fatal("binding rewrote prose")
	}
	base["extra.yaml"] = treeFile(testGaggle)
	if _, err := Bind(base, "orders"); err == nil {
		t.Fatal("accepted multi-gaggle package")
	}
}

func TestUnsafeTreesRejected(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", "a/../../outside", `a\b`, "C:escape", ".template/lock.json", ".git/config", "a//b"} {
		if err := (Tree{name: treeFile("bad")}).Validate(); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if err := (Tree{"A.md": treeFile("a"), "a.md": treeFile("b")}).Validate(); err == nil {
		t.Fatal("accepted portable path collision")
	}
}

func TestBindRejectsAmbiguousYAML(t *testing.T) {
	for _, text := range []string{
		testGaggle + "---\n" + testGaggle,
		strings.Replace(testGaggle, "namespace: example", "namespace: &name example, key: *name", 1),
		strings.Replace(testGaggle, "name: example", "name: example\n  name: duplicate", 1),
	} {
		if _, err := Bind(Tree{"gaggle.yaml": treeFile(text)}, "orders"); err == nil {
			t.Fatalf("ambiguous YAML accepted: %s", text)
		}
	}
}

func trackedFixture(t *testing.T, root string) Tree {
	t.Helper()
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := (Tree{"gaggle.yaml": treeFile(testGaggle)}).Write(root); err != nil {
		t.Fatal(err)
	}
	base, err := ReadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(root, testSource(), testRevision, base); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestTrackingRoundTripAndCorruption(t *testing.T) {
	root := t.TempDir()
	if tracking, err := Load(root); err != nil || tracking != nil {
		t.Fatalf("legacy directory: %v %v", tracking, err)
	}
	base := trackedFixture(t, root)
	tracking, err := Load(root)
	if err != nil || tracking.Lock.Digest != base.Digest() {
		t.Fatalf("load: %v %v", tracking, err)
	}
	current, err := ReadTree(root)
	if err != nil || current.Digest() != base.Digest() {
		t.Fatal("management documents leaked into template contents")
	}
	if err := os.WriteFile(filepath.Join(root, MetadataDir, "lock.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Fatal("corrupt lock accepted")
	}
}

func TestDeploymentGuard(t *testing.T) {
	current, candidate := t.TempDir(), t.TempDir()
	gaggle := filepath.Join(current, "gaggles", "orders")
	base := trackedFixture(t, gaggle)
	if err := GuardReplacement(current, candidate); err == nil {
		t.Fatal("missing deployment ancestry accepted")
	}
	if err := RecordDeployments(current); err != nil {
		t.Fatal(err)
	}
	if err := GuardReplacement(current, candidate); err != nil {
		t.Fatalf("unchanged gaggle removal should be allowed: %v", err)
	}
	file := base["gaggle.yaml"]
	file.Data = []byte(strings.ReplaceAll(string(file.Data), "name: example", "name: customized"))
	base["gaggle.yaml"] = file
	if err := base.Write(gaggle); err != nil {
		t.Fatal(err)
	}
	if err := GuardReplacement(current, candidate); err == nil {
		t.Fatal("pending runtime edits were lost")
	}
	if err := base.Write(filepath.Join(candidate, "gaggles", "orders")); err != nil {
		t.Fatal(err)
	}
	if err := GuardReplacement(current, candidate); err != nil {
		t.Fatalf("persisted edits rejected: %v", err)
	}
}

func TestPublishChecksConcurrentChangesAndRecovery(t *testing.T) {
	parent := t.TempDir()
	target, candidate := filepath.Join(parent, "orders"), filepath.Join(parent, ".candidate")
	base := trackedFixture(t, target)
	if err := base.Write(candidate); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "new.md"), []byte("concurrent edit"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Publish(target, candidate, base); err == nil {
		t.Fatal("concurrent edit overwritten")
	}
	current, err := ReadTree(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(parent, ".template-backup-orders"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := Publish(target, candidate, current); err == nil {
		t.Fatal("interrupted publication ignored")
	}
}

func TestLockAndStatusLegacyAndManaged(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	release, err := LockConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".template-config.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("legacy path created lock state")
	}
	if status := InventoryStatus(root, "orders"); status != nil {
		t.Fatal("legacy gaggle gained template status")
	}
	trackedFixture(t, filepath.Join(config, "gaggles", "orders"))
	release, err = LockConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockConfig(config); err == nil {
		t.Fatal("second concurrent writer accepted")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	if err := WriteStatus(root, "orders", Status{State: "update-available", Installed: testRevision, CheckedAt: now, LastSuccess: now}); err != nil {
		t.Fatal(err)
	}
	if status := InventoryStatus(root, "orders"); status == nil || status.State != "update-available" {
		t.Fatalf("status=%+v", status)
	}
}
