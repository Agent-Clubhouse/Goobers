package mutationsidecar

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type diagnosticFact struct {
	Name string `json:"name"`
}

func TestReadFactsMissingAndBlank(t *testing.T) {
	root := t.TempDir()
	facts, issues := ReadFacts[diagnosticFact](root, nil)
	if facts != nil || issues != nil {
		t.Fatalf("missing sidecar = %+v, issues=%v", facts, issues)
	}

	if err := os.WriteFile(filepath.Join(root, FileName), []byte("\n \t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	facts, issues = ReadFacts[diagnosticFact](root, nil)
	if len(facts) != 0 || len(issues) != 0 {
		t.Fatalf("blank sidecar = %+v, issues=%v", facts, issues)
	}
}

func TestReadFactsContinuesAfterMalformedJSON(t *testing.T) {
	root := t.TempDir()
	data := []byte("{\"name\":\"one\"}\n\n{\n {\"name\":\"two\"} \n")
	if err := os.WriteFile(filepath.Join(root, FileName), data, 0o600); err != nil {
		t.Fatal(err)
	}

	facts, issues := ReadFacts[diagnosticFact](root, nil)
	wantFacts := []diagnosticFact{{Name: "one"}, {Name: "two"}}
	wantIssues := []string{"line 3: unexpected end of JSON input"}
	if !reflect.DeepEqual(facts, wantFacts) || !reflect.DeepEqual(issues, wantIssues) {
		t.Fatalf("facts = %+v, issues=%v; want %+v, %v", facts, issues, wantFacts, wantIssues)
	}
}

func TestReadFactsReportsReadErrors(t *testing.T) {
	t.Run("nonregular file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, FileName), 0o700); err != nil {
			t.Fatal(err)
		}
		facts, issues := ReadFacts[diagnosticFact](root, nil)
		if facts != nil || len(issues) != 1 || !strings.HasPrefix(issues[0], "read sidecar: ") {
			t.Fatalf("facts = %+v, issues=%v", facts, issues)
		}
	})

	t.Run("oversized input", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, FileName)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, MaxBytes+1); err != nil {
			t.Fatal(err)
		}
		facts, issues := ReadFacts[diagnosticFact](root, nil)
		want := []string{fmt.Sprintf("read sidecar: mutation sidecar exceeds %d bytes", MaxBytes)}
		if facts != nil || !reflect.DeepEqual(issues, want) {
			t.Fatalf("facts = %+v, issues=%v; want nil, %v", facts, issues, want)
		}
	})
}

func TestReadFactsReportsValidationFailures(t *testing.T) {
	root := t.TempDir()
	data := []byte("\n{\"name\":\"\"}\n{\"name\":\"valid\"}\n")
	if err := os.WriteFile(filepath.Join(root, FileName), data, 0o600); err != nil {
		t.Fatal(err)
	}

	facts, issues := ReadFacts(root, func(line int, fact diagnosticFact) string {
		if fact.Name == "" {
			return fmt.Sprintf("name is required at source line %d", line)
		}
		return ""
	})
	wantFacts := []diagnosticFact{{Name: "valid"}}
	wantIssues := []string{"line 2: name is required at source line 2"}
	if !reflect.DeepEqual(facts, wantFacts) || !reflect.DeepEqual(issues, wantIssues) {
		t.Fatalf("facts = %+v, issues=%v; want %+v, %v", facts, issues, wantFacts, wantIssues)
	}
}
