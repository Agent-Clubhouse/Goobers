package workerhost

import "testing"

func TestShortSHA(t *testing.T) {
	full := "0123456789abcdef0123456789abcdef01234567"
	for in, want := range map[string]string{"": "(absent)", "abc": "abc", full: "01234567"} {
		if got := shortSHA(in); got != want {
			t.Errorf("shortSHA(%q) = %q, want %q", in, got, want)
		}
	}
}
