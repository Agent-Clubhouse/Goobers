package journal

import (
	"bytes"
	"strings"
	"testing"
)

func FuzzCheckpointScrubberChunkBoundaries(f *testing.F) {
	for _, seed := range []string{
		"ordinary output",
		"registered-secret abcabcabcabcabc",
		"ghp_" + strings.Repeat("A", 80),
		"-----BEGIN PRIVATE KEY-----\nbody\n-----END PRIVATE KEY-----\nmore output",
		"-----BEGIN PRIVATE KEY-----\nunfinished",
		"bearer " + strings.Repeat("a", 80),
	} {
		f.Add([]byte(seed), uint8(3))
	}
	f.Fuzz(func(t *testing.T, data []byte, width uint8) {
		if len(data) > 4096 {
			return
		}
		registry, scrubber := DefaultScrubber()
		registry.Register([]byte("registered-secret"))
		registry.Register([]byte("abcabcabc"))
		stream, err := NewCheckpointScrubber(scrubber)
		if err != nil {
			t.Fatal(err)
		}
		input := append(bytes.Clone(data), '\n', '!', '\n')
		var got []byte
		for start := 0; start < len(input); {
			end := min(start+int(width)+1, len(input))
			got = append(got, scrubCheckpointDelta(t, stream, input[start:end])...)
			start = end
		}
		// An unfinished PEM may retain its suffix indefinitely, but persisted
		// bytes must always agree with the fully scrubbed stream's prefix.
		if !bytes.HasPrefix(scrubber.Scrub(input), got) {
			t.Fatal("checkpoint output is not a safe whole-scrub prefix")
		}
	})
}
