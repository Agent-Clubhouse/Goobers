package journal

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckpointScrubberEveryRegisteredSecretSplit(t *testing.T) {
	for _, secret := range []string{"secret-value", "abcabcabc", "value\nwith\"<escapes>"} {
		registry, scrubber := DefaultScrubber()
		registry.Register([]byte(secret))
		encoded, err := json.Marshal(secret)
		if err != nil {
			t.Fatal(err)
		}
		input := []byte("output: " + secret + "\nJSON: " + string(encoded) + "\ndone.\n")
		for split := range len(input) + 1 {
			stream, err := NewCheckpointScrubber(scrubber)
			if err != nil {
				t.Fatal(err)
			}
			got := bytes.Clone(scrubCheckpointDelta(t, stream, input[:split]))
			got = append(got, scrubCheckpointDelta(t, stream, input[split:])...)
			if !bytes.Equal(got, scrubber.Scrub(input)) {
				t.Fatalf("checkpoint differs from whole scrub at split %d", split)
			}
		}
	}
}

func TestCheckpointScrubberBytewisePatternAndRegistryChain(t *testing.T) {
	registry, scrubber := DefaultScrubber()
	registry.Register([]byte("exact-secret"))
	stream, err := NewCheckpointScrubber(scrubber)
	if err != nil {
		t.Fatal(err)
	}
	input := []byte("output: exact-secret ghp_" + strings.Repeat("A", 80) + "\n-----BEGIN PRIVATE KEY-----\nbody\n-----END PRIVATE KEY-----\ndone.\n")
	var got []byte
	for i := range input {
		got = append(got, scrubCheckpointDelta(t, stream, input[i:i+1])...)
	}
	if !bytes.Equal(got, scrubber.Scrub(input)) {
		t.Fatalf("bytewise chain differs from whole-input scrub: got=%q want=%q", got, scrubber.Scrub(input))
	}
}

func TestCheckpointScrubberRetainsUnfinishedSecret(t *testing.T) {
	registry, scrubber := DefaultScrubber()
	registry.Register([]byte("exact-secret"))
	stream, err := NewCheckpointScrubber(scrubber)
	if err != nil {
		t.Fatal(err)
	}
	if got := scrubCheckpointDelta(t, stream, []byte("output: exact-sec")); string(got) != "output: " {
		t.Fatalf("unfinished secret persisted: %q", got)
	}
	if got := scrubCheckpointDelta(t, stream, []byte("ret\n")); string(got) != Redacted+"\n" {
		t.Fatalf("completed secret not redacted: %q", got)
	}
}

type opaqueCheckpointScrubber struct{}

func (opaqueCheckpointScrubber) Scrub(input []byte) []byte { return input }

func TestCheckpointScrubberRejectsOpaqueMember(t *testing.T) {
	if _, err := NewCheckpointScrubber(Chain(NewPatternScrubber(), opaqueCheckpointScrubber{})); err == nil {
		t.Fatal("opaque scrubber accepted for checkpointing")
	}
}

func scrubCheckpointDelta(t *testing.T, stream *CheckpointScrubber, delta []byte) []byte {
	t.Helper()
	out, err := stream.ScrubDelta(delta)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCheckpointScrubberBoundsCaptureBeforeAllocation(t *testing.T) {
	stream, err := NewCheckpointScrubber(nopScrubber{})
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for range MaxCheckpointScrubBytes / len(chunk) {
		if got := scrubCheckpointDelta(t, stream, chunk); !bytes.Equal(got, chunk) {
			t.Fatal("bounded capture lost bytes")
		}
	}
	for range 3 {
		if got, err := stream.ScrubDelta([]byte("overflow")); err == nil || len(got) != 0 {
			t.Fatal("over-limit capture accepted")
		}
	}
	if stream.bytes != MaxCheckpointScrubBytes || len(stream.stages[0].pending) != 0 {
		t.Fatal("rejected input mutated retained capture")
	}
}
