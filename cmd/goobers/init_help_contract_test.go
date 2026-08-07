package main

import (
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

// TestInitHelpMatchesScaffold is the help/scaffold contract for #2446: the
// init help must name every top-level entry a real `goobers init` creates,
// and must not claim top-level runs/ or workcopies/ — the daemon creates
// those later, gaggle-scoped under gaggles/<gaggle>/ — so any mention of
// them is allowed only inside that runtime explanation.
func TestInitHelpMatchesScaffold(t *testing.T) {
	root := t.TempDir()
	if _, err := instance.Init(root); err != nil {
		t.Fatalf("instance.Init: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("scaffold created no top-level entries")
	}
	created := make(map[string]bool, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		created[name] = true
		if !strings.Contains(initHelp, name) {
			t.Errorf("init help does not mention scaffolded top-level entry %q", name)
		}
	}

	for _, dir := range []string{instance.RunsDirName, instance.WorkcopiesDirName} {
		if created[dir+"/"] {
			t.Errorf("scaffold now creates top-level %s/ — reconcile init help and this test", dir)
		}
		// The only sentence allowed to mention runs/ or workcopies/ is the
		// runtime explanation: the one naming the daemon and the per-gaggle
		// home under gaggles/. A revert to the old "init scaffolds runs/ and
		// workcopies/" claim puts them in a sentence without either and fails.
		for _, sentence := range helpSentences(initHelp) {
			if !strings.Contains(sentence, dir+"/") {
				continue
			}
			if !strings.Contains(sentence, "daemon") || !strings.Contains(sentence, instance.GagglesDirName+"/") {
				t.Errorf("init help claims %s/ outside the daemon runtime explanation: %q", dir, sentence)
			}
		}
	}
}

// helpSentences splits flowed help text into sentences on ". " boundaries
// (dotted names like instance.yaml and telemetry.db never precede a space,
// so they do not split).
func helpSentences(help string) []string {
	return strings.SplitAfter(strings.ReplaceAll(help, "\n", " "), ". ")
}
