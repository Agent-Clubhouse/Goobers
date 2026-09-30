package secretpattern

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// Keep the original replacement sequence as a differential oracle. The
// optimization must change allocation, never pattern ordering or redaction.
func scrubReference(s *Scrubber, input []byte) []byte {
	for _, p := range s.patterns {
		input = p.re.ReplaceAll(input, []byte(p.replacement))
	}
	return input
}

func FuzzScrubMatchesReference(f *testing.F) {
	for _, input := range []string{
		"", "ordinary output", strings.Repeat("x", 32<<10),
		"ghp_" + strings.Repeat("a", 36),
		"github_pat_" + strings.Repeat("a", 50),
		"AKIA" + strings.Repeat("A", 16),
		"xoxb-" + strings.Repeat("a", 20),
		"sk-ant-" + strings.Repeat("a", 20),
		"glpat-" + strings.Repeat("a", 20),
		"eyJabc.def.ghi",
		"-----BEGIN RSA PRIVATE KEY-----\nsecret\n-----END RSA PRIVATE KEY-----",
		"Bearer \n" + strings.Repeat("a", 24) + "==",
		"BaſIC " + strings.Repeat("a", 24) + "==",
		"\xff\x00ghp_" + strings.Repeat("a", 36) + "\xfe",
		"ghp_" + strings.Repeat("a", 36) + " Basic " + strings.Repeat("b", 24),
	} {
		f.Add([]byte(input))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 64<<10 {
			return
		}
		s := NewScrubber()
		original := bytes.Clone(input)
		want := scrubReference(s, input)
		got := s.Scrub(input)
		if !bytes.Equal(got, want) {
			t.Fatal("optimized redaction differs from original replacement sequence")
		}
		if !bytes.Equal(input, original) {
			t.Fatal("scrubbing changed caller's input")
		}
		if len(got) > 0 {
			got[0] ^= 1
			if !bytes.Equal(input, original) {
				t.Fatal("output aliases caller's input")
			}
		}
	})
}

func BenchmarkScrubPayload(b *testing.B) {
	for _, size := range []int{1024, 32 << 10} {
		for _, token := range []string{"ordinary", "late-token"} {
			input := []byte(strings.Repeat("x", size))
			if token == "late-token" {
				input = append(input, []byte(" Bearer "+strings.Repeat("a", 24))...)
			}
			s := NewScrubber()
			for _, implementation := range []string{"reference", "production"} {
				b.Run(strconv.Itoa(size)+"/"+token+"/"+implementation, func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(input)))
					for b.Loop() {
						if implementation == "reference" {
							_ = scrubReference(s, input)
						} else {
							_ = s.Scrub(input)
						}
					}
				})
			}
		}
	}
}
