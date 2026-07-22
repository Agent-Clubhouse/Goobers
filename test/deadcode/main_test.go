package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseExemptionsRequiresExactSymbolAndReason(t *testing.T) {
	t.Parallel()
	got, err := parseExemptions(strings.NewReader(`
# Reviewed compatibility surfaces.
github.com/goobers/goobers/api/v1alpha1.Resource # Kubernetes API helper retained for consumers.
github.com/goobers/goobers/internal/testdep.Require # Called only from integration-tagged tests.
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"github.com/goobers/goobers/api/v1alpha1.Resource":    "Kubernetes API helper retained for consumers.",
		"github.com/goobers/goobers/internal/testdep.Require": "Called only from integration-tagged tests.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exemptions = %#v, want %#v", got, want)
	}

	for _, input := range []string{
		"github.com/goobers/goobers/internal/testdep.Require",
		"github.com/goobers/goobers/internal/testdep.Require # ",
		"github.com/goobers/goobers/internal/testdep.Require # reason\n" +
			"github.com/goobers/goobers/internal/testdep.Require # duplicate",
	} {
		if _, err := parseExemptions(strings.NewReader(input)); err == nil {
			t.Errorf("parseExemptions(%q) succeeded, want error", input)
		}
	}
}

func TestExemptionProblemsRejectsNewAndStaleEntries(t *testing.T) {
	t.Parallel()
	reports := []reportPackage{{
		Path: "github.com/goobers/goobers/internal/example",
		Funcs: []reportFunction{
			{Name: "Reviewed", Position: reportPosition{File: "internal/example/example.go", Line: 10, Col: 6}},
			{Name: "NewFinding", Position: reportPosition{File: "internal/example/example.go", Line: 20, Col: 6}},
		},
	}}
	exemptions := map[string]string{
		"github.com/goobers/goobers/internal/example.Reviewed": "Intentional extension seam.",
		"github.com/goobers/goobers/internal/example.Removed":  "Old extension seam.",
	}

	got := exemptionProblems(reports, exemptions)
	want := []string{
		"internal/example/example.go:20:6: unreviewed dead code: github.com/goobers/goobers/internal/example.NewFinding",
		"stale deadcode exemption: github.com/goobers/goobers/internal/example.Removed (Old extension seam.)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("problems = %q, want %q", got, want)
	}
}

func TestDecodeReportsMatchesDeadcodeSchema(t *testing.T) {
	t.Parallel()
	reports, err := decodeReports([]byte(`[
		{"Path":"github.com/goobers/goobers/internal/example","Funcs":[
			{"Name":"Unused","Position":{"File":"example.go","Line":7,"Col":6}}
		]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if got := reports[0].Funcs[0]; got.Name != "Unused" || got.Position.Line != 7 {
		t.Fatalf("decoded function = %#v", got)
	}
}
