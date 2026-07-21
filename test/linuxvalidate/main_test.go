//go:build unix

package main

import "testing"

func TestRunIDPattern(t *testing.T) {
	out := "created run a671b69fe766595e550677b91658726a (workflow=demo gaggle=demo)\nfinished: phase=completed\n"
	m := runIDPattern.FindStringSubmatch(out)
	if m == nil {
		t.Fatal("expected a run id match")
	}
	if got, want := m[1], "a671b69fe766595e550677b91658726a"; got != want {
		t.Fatalf("run id = %q, want %q", got, want)
	}
}

func TestRunIDPatternNoMatch(t *testing.T) {
	if runIDPattern.FindStringSubmatch("no run here") != nil {
		t.Fatal("did not expect a match")
	}
}

func TestFirstLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"one\ntwo\n", "one"},
		{"  solo  ", "solo"},
		{"", ""},
	} {
		if got := firstLine(tc.in); got != tc.want {
			t.Fatalf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
