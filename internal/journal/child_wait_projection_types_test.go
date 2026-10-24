package journal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// declaredEventTypes is every EventType constant this package declares.
func declaredEventTypes(t *testing.T) []EventType {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var types []EventType
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "EventType" {
					continue
				}
				for _, v := range value.Values {
					if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						s, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatal(err)
						}
						types = append(types, EventType(s))
					}
				}
			}
		}
	}
	if len(types) < 20 {
		t.Fatalf("found only %d EventType constants; the scan is broken", len(types))
	}
	return types
}

// A reader that selects only ChildWaitProjectionTypes from storage gets the
// same projection as from the whole journal: every other declared event type,
// interleaved anywhere in a child-wait history, changes nothing. A projector
// that starts reading a new type fails here until the list names it.
func TestChildWaitProjectionTypesAreExactlyWhatTheProjectorReads(t *testing.T) {
	read := ChildWaitProjectionTypes()
	events, _, _ := parallelWaitFixture()
	// A branch-filtered history: no parallel declaration, so branch.started
	// alone names the owner, and dropping it would add a runnable root.
	filtered := []Event{{Type: EventBranchStarted, Branch: 1}, events[1], events[2]}
	if p, err := ProjectChildWaits(filtered); err != nil || !p.Parked() {
		t.Fatalf("branch-filtered fixture = %+v, %v; want parked", p, err)
	}
	histories := [][]Event{events, events[:3], events[:4], filtered}
	for _, kind := range declaredEventTypes(t) {
		if slices.Contains(read, kind) {
			continue
		}
		for h, history := range histories {
			want, wantErr := ProjectChildWaits(history)
			for at := 0; at <= len(history); at++ {
				noisy := append(append(append([]Event{}, history[:at]...), Event{Type: kind, Branch: 1, Stage: "stage-1", Attempt: 1, Parallel: "fan"}), history[at:]...)
				got, err := ProjectChildWaits(noisy)
				if (err == nil) != (wantErr == nil) || !reflect.DeepEqual(got, want) {
					t.Fatalf("a %s event at %d in history %d changed the projection: %+v (%v), want %+v (%v); add it to ChildWaitProjectionTypes", kind, at, h, got, err, want, wantErr)
				}
			}
		}
	}
	// Selecting only the listed types keeps every history's projection.
	for h, history := range histories {
		var selected []Event
		for _, event := range history {
			if slices.Contains(read, event.Type) {
				selected = append(selected, event)
			}
		}
		want, wantErr := ProjectChildWaits(history)
		got, err := ProjectChildWaits(selected)
		if (err == nil) != (wantErr == nil) || !reflect.DeepEqual(got, want) {
			t.Fatalf("history %d selected by type projects %+v (%v), want %+v (%v)", h, got, err, want, wantErr)
		}
	}
	// Every listed type is one the projector depends on.
	for _, kind := range read {
		if !slices.Contains(declaredEventTypes(t), kind) {
			t.Fatalf("ChildWaitProjectionTypes names undeclared type %q", kind)
		}
	}
}
