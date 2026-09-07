package providers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// TestAllCapabilitiesCoversEveryDeclaredConstant checks AllCapabilities()
// against an INDEPENDENT source: the Capability constants declared in
// capability.go, read from that file's AST.
//
// This exists because the previous guard compared the hand-maintained list to
// itself — it asserted len(cells) == len(AllCapabilities())*len(providers),
// which holds no matter how many constants the list omits. repo.push.preflight
// and ci.cancel were declared and used while missing from the list, so they
// appeared in neither the generated provider-capability matrix nor
// ValidateBlessedTier, and CI stayed green (#2061/#2179).
func TestAllCapabilitiesCoversEveryDeclaredConstant(t *testing.T) {
	t.Parallel()

	declared := declaredCapabilityConstants(t)
	listed := map[Capability]bool{}
	for _, capability := range AllCapabilities() {
		if listed[capability] {
			t.Errorf("AllCapabilities lists %q twice", capability)
		}
		listed[capability] = true
	}

	for _, name := range sortedConstantNames(declared) {
		value := declared[name]
		if !listed[value] {
			t.Errorf("capability.go declares %s = %q but AllCapabilities omits it; "+
				"it would be missing from docs/provider-capability-matrix.md and from ValidateBlessedTier", name, value)
		}
	}

	byValue := map[Capability]bool{}
	for _, value := range declared {
		byValue[value] = true
	}
	for capability := range listed {
		if !byValue[capability] {
			t.Errorf("AllCapabilities lists %q, which capability.go does not declare as a Capability constant", capability)
		}
	}
}

// declaredCapabilityConstants parses capability.go and returns every constant
// whose declared type is Capability, keyed by its Go identifier.
func declaredCapabilityConstants(t *testing.T) map[string]Capability {
	t.Helper()

	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "capability.go", nil, 0)
	if err != nil {
		t.Fatalf("parse capability.go: %v", err)
	}

	found := map[string]Capability{}
	for _, decl := range file.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := value.Type.(*ast.Ident)
			if !ok || ident.Name != "Capability" {
				continue
			}
			for i, name := range value.Names {
				if i >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Errorf("%s is a Capability constant but not a plain string literal; the completeness check cannot read it", name.Name)
					continue
				}
				found[name.Name] = Capability(literal.Value[1 : len(literal.Value)-1])
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("parsed no Capability constants from capability.go; the check would pass vacuously")
	}
	return found
}

func sortedConstantNames(declared map[string]Capability) []string {
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
