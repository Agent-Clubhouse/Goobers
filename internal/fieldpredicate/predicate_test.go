package fieldpredicate

import (
	"strings"
	"testing"
)

func TestPredicateMatchesTypedFields(t *testing.T) {
	predicate, err := Compile(`fields["System.Priority"] <= 2 && fields["System.WorkItemType"] == "Bug" && fields["System.Blocked"] != true`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	matched, err := predicate.Matches(Fields{
		"System.Priority":     float64(1),
		"System.WorkItemType": "Bug",
		"System.Blocked":      false,
	})
	if err != nil {
		t.Fatalf("Matches: %v", err)
	}
	if !matched {
		t.Fatal("Matches = false, want true")
	}
}

func TestPredicateUnavailableFieldFailsExplicitly(t *testing.T) {
	predicate, err := Compile(`fields["milestone.title"] == "V1"`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	_, err = predicate.Matches(Fields{"state": "open"})
	if err == nil || !strings.Contains(err.Error(), `field "milestone.title" is unavailable`) {
		t.Fatalf("Matches error = %v, want unavailable-field error", err)
	}
}

func TestCompileConjunctionRequiresEveryPredicate(t *testing.T) {
	predicate, err := CompileConjunction(
		`fields["state"] == "open"`,
		`fields["number"] >= 10`,
	)
	if err != nil {
		t.Fatalf("CompileConjunction: %v", err)
	}
	for _, tt := range []struct {
		name   string
		fields Fields
		want   bool
	}{
		{name: "both match", fields: Fields{"state": "open", "number": int64(10)}, want: true},
		{name: "first rejects", fields: Fields{"state": "closed", "number": int64(10)}},
		{name: "second rejects", fields: Fields{"state": "open", "number": int64(9)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := predicate.Matches(tt.fields)
			if err != nil {
				t.Fatalf("Matches: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Matches = %v, want %v", got, tt.want)
			}
		})
	}
	if _, err := predicate.Matches(Fields{"state": "closed"}); err == nil ||
		!strings.Contains(err.Error(), `field "number" is unavailable`) {
		t.Fatalf("short-circuited unavailable field error = %v", err)
	}
}

func TestPredicateRejectsUnsupportedCEL(t *testing.T) {
	for _, expression := range []string{
		`fields["priority"]`,
		`fields.priority == 1`,
		`fields["priority"] + 1 == 2`,
		`fields["priority"] > fields["other"]`,
		`2 < fields["priority"]`,
		`true`,
	} {
		t.Run(expression, func(t *testing.T) {
			_, err := Compile(expression)
			if err == nil {
				t.Fatalf("Compile(%q) succeeded, want validation error", expression)
			}
		})
	}
}

func TestOrderComparesMultipleTypedFields(t *testing.T) {
	order, err := ParseOrder("priority:desc,title")
	if err != nil {
		t.Fatalf("ParseOrder: %v", err)
	}
	items := []Fields{
		{"priority": 1, "title": "Beta"},
		{"priority": float64(2), "title": "Zulu"},
		{"priority": int64(2), "title": "Alpha"},
	}
	if err := order.Validate(items); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got, err := order.Compare(items[1], items[0]); err != nil || got >= 0 {
		t.Fatalf("Compare(high, low) = %d, %v; want high first", got, err)
	}
	if got, err := order.Compare(items[2], items[1]); err != nil || got >= 0 {
		t.Fatalf("Compare(alpha, zulu) = %d, %v; want alpha first within priority", got, err)
	}
}

func TestOrderUnavailableAndInvalidFieldsFailExplicitly(t *testing.T) {
	order, err := ParseOrder("priority:asc")
	if err != nil {
		t.Fatalf("ParseOrder: %v", err)
	}
	if err := order.Validate([]Fields{{"priority": 1}, {}}); err == nil ||
		!strings.Contains(err.Error(), `field "priority" is unavailable`) {
		t.Fatalf("Validate error = %v, want unavailable-field error", err)
	}
	if _, err := ParseOrder("priority:sideways"); err == nil {
		t.Fatal("ParseOrder accepted unsupported direction")
	}
}
