package textsuggest

import "testing"

func TestDistance(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want int
	}{
		{name: "empty", a: "", b: "", want: 0},
		{name: "insertions", a: "", b: "help", want: 4},
		{name: "same", a: "workflow", b: "workflow", want: 0},
		{name: "ASCII edits", a: "kitten", b: "sitting", want: 3},
		{name: "UTF-8 bytes", a: "é", b: "e", want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Distance(tt.a, tt.b); got != tt.want {
				t.Fatalf("Distance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
