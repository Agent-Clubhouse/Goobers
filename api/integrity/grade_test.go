package integrity

import "testing"

func TestGradeMeetsMinimum(t *testing.T) {
	tests := []struct {
		actual  Grade
		minimum Grade
		want    bool
	}{
		{Trusted, Trusted, true},
		{Maintainer, Trusted, false},
		{Derived, Maintainer, true},
		{Maintainer, Derived, true},
		{Unapproved, Maintainer, false},
		{Unapproved, Unapproved, true},
	}
	for _, test := range tests {
		if got := test.actual.Meets(test.minimum); got != test.want {
			t.Errorf("%q.Meets(%q) = %t, want %t", test.actual, test.minimum, got, test.want)
		}
	}
}

func TestWeakest(t *testing.T) {
	tests := []struct {
		name   string
		grades []Grade
		want   Grade
	}{
		{name: "trusted", grades: []Grade{Trusted}, want: Trusted},
		{name: "maintainer beats trusted", grades: []Grade{Trusted, Maintainer}, want: Maintainer},
		{name: "derived remains distinct", grades: []Grade{Maintainer, Derived}, want: Derived},
		{name: "unapproved wins", grades: []Grade{Trusted, Derived, Unapproved}, want: Unapproved},
		{name: "empty", want: ""},
		{name: "invalid", grades: []Grade{Trusted, "unknown"}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Weakest(test.grades...); got != test.want {
				t.Fatalf("Weakest(%v) = %q, want %q", test.grades, got, test.want)
			}
		})
	}
}
