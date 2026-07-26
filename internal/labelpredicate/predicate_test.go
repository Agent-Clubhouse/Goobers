package labelpredicate

import (
	"strings"
	"testing"
)

func TestPredicateGroupedBooleanComposition(t *testing.T) {
	predicate, err := Compile(
		`("size:s" in labels || "size:m" in labels) && !("platform:windows" in labels)`,
		[]string{"area:runner"},
		[]string{"goobers:claimed"},
	)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{name: "matching small item", labels: []string{"area:runner", "size:s"}, want: true},
		{name: "matching medium item", labels: []string{"area:runner", "size:m"}, want: true},
		{name: "wrong size", labels: []string{"area:runner", "size:l"}},
		{name: "negated label", labels: []string{"area:runner", "size:s", "platform:windows"}},
		{name: "missing legacy required", labels: []string{"size:s"}},
		{name: "legacy excluded", labels: []string{"area:runner", "size:s", "goobers:claimed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := predicate.Matches(tt.labels)
			if err != nil {
				t.Fatalf("Matches: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Matches(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestPredicateRejectsUnsupportedCEL(t *testing.T) {
	tests := []string{
		`labels.size() > 0`,
		`"size:s" in labels ? true : false`,
		`labels.exists(label, label == "size:s")`,
		`true`,
	}
	for _, expression := range tests {
		t.Run(expression, func(t *testing.T) {
			_, err := Compile(expression, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "unsupported CEL expression") {
				t.Fatalf("Compile(%q) error = %v, want unsupported-expression error", expression, err)
			}
		})
	}
}

func TestPredicateRejectsInvalidCEL(t *testing.T) {
	_, err := Compile(`"size:s" in`, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "compile CEL expression") {
		t.Fatalf("Compile error = %v, want compile error", err)
	}
}

func TestPredicateRejectsBlankCEL(t *testing.T) {
	_, err := Compile(" \t\n", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "must not be blank") {
		t.Fatalf("Compile error = %v, want blank-expression error", err)
	}
}

func TestPredicateRequiredLabelsAreCopied(t *testing.T) {
	required := []string{"goobers:ready"}
	predicate, err := Compile("", required, []string{"goobers:claimed"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	required[0] = "changed"
	got := predicate.RequiredLabels()
	got[0] = "also-changed"
	if again := predicate.RequiredLabels(); len(again) != 1 || again[0] != "goobers:ready" {
		t.Fatalf("RequiredLabels() = %v, want an immutable copy", again)
	}
}
