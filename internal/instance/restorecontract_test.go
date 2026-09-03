package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The restore contract (docs/guides/instance-restore-contract.md) classifies
// every instance-root path as must-survive, regenerable, or transient. A new
// layout name that never reaches that table is an operator restoring a root
// without knowing whether the path they skipped was safe to skip, so the
// classification is checked against the layout constants rather than trusted to
// stay current on its own.
const restoreContractDoc = "instance-restore-contract.md"

var restoreContractClasses = map[string]bool{
	"must-survive": true,
	"regenerable":  true,
	"transient":    true,
}

func TestRestoreContractClassifiesEveryLayoutPath(t *testing.T) {
	rows := restoreContractRows(t)

	for _, name := range []string{
		ConfigFileName,
		ConfigDirName,
		GagglesDirName,
		RunsDirName,
		WorkcopiesDirName,
		SchedulerDirName,
		DocsUpdaterDirName,
		TutorHoldoutsDirName,
		BacklogHealthDirName,
		BlobStoreDirName,
		TelemetryDBName,
		ReadDBName,
		IntakeDBName,
	} {
		classified := false
		for path := range rows {
			if strings.Contains(path, name) {
				classified = true
				break
			}
		}
		if !classified {
			t.Errorf("%s does not classify %q", restoreContractDoc, name)
		}
	}
}

func TestRestoreContractUsesTheDurabilityClassEnum(t *testing.T) {
	for path, class := range restoreContractRows(t) {
		// A row may qualify its class ("must-survive, as one set"); the class
		// itself is the leading term.
		leading, _, _ := strings.Cut(class, ",")
		if !restoreContractClasses[strings.TrimSpace(leading)] {
			t.Errorf("%s row %q has class %q outside the enum", restoreContractDoc, path, class)
		}
	}
}

// restoreContractRows maps each classification-table row's path cell to its
// class cell. Rows before the table, and the table's own header and separator,
// are skipped.
func restoreContractRows(t *testing.T) map[string]string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join("..", "..", "docs", "guides", restoreContractDoc))
	if err != nil {
		t.Fatal(err)
	}

	rows := map[string]string{}
	inTable := false
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			inTable = strings.Contains(line, "The classification")
			continue
		}
		if !inTable || !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 3 {
			continue
		}
		path := strings.TrimSpace(cells[0])
		class := strings.TrimSpace(strings.ReplaceAll(cells[1], "**", ""))
		if path == "Path" || strings.HasPrefix(path, "---") {
			continue
		}
		rows[path] = class
	}
	if len(rows) == 0 {
		t.Fatalf("%s has no classification table rows", restoreContractDoc)
	}
	return rows
}
