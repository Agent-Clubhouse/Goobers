package truncate

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBytes(t *testing.T) {
	if got := Bytes("abc", 3, "..."); got != "abc" {
		t.Fatalf("got %q", got)
	}
	for _, unit := range []string{"é", "世", "😀"} {
		for limit := 4; limit < 30; limit++ {
			in := strings.Repeat(unit, 40)
			got := Bytes(in, limit, "...")
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8 for %q limit %d: %q", unit, limit, got)
			}
			if len(got) > limit {
				t.Fatalf("len %d exceeds %d", len(got), limit)
			}
			if !strings.HasSuffix(got, "...") {
				t.Fatalf("missing suffix: %q", got)
			}
		}
	}
	if got := Bytes("😀😀", 2, "..."); got != "" && !utf8.ValidString(got) {
		t.Fatalf("got %q", got)
	}
}
