package artifactset

import (
	"bytes"
	"context"
	"runtime/trace"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestSanitizeNativeGoTrace(t *testing.T) {
	var captured bytes.Buffer
	if err := trace.Start(&captured); err != nil {
		t.Fatal(err)
	}
	trace.Log(context.Background(), "artifact-test", "private-trace-credential")
	trace.Stop()
	raw := captured.Bytes()
	if !bytes.Contains(raw, []byte("private-trace-credential")) {
		t.Fatal("trace fixture lacks its test credential")
	}
	scrubber := journal.NewRegistryScrubber()
	clean, err := NewSanitizer(scrubber)("application/x-go-trace", raw)
	if err != nil || !bytes.Equal(clean, raw) {
		t.Fatalf("native trace not preserved: %v", err)
	}
	clean[0] = 'X'
	if raw[0] != 'g' {
		t.Fatal("sanitized trace aliases source bytes")
	}
	scrubber.Register([]byte("private-trace-credential"))
	if _, err := NewSanitizer(scrubber)("application/x-go-trace", raw); err == nil || bytes.Contains([]byte(err.Error()), []byte("private-trace-credential")) {
		t.Fatalf("unsafe trace accepted or leaked in error: %v", err)
	}
	for _, malformed := range [][]byte{
		[]byte("opaque"),
		[]byte("go 1.21 trace\x00\x00\x00"),
		raw[:16],
		raw[:len(raw)/2],
		append(bytes.Clone(raw), 0xff),
	} {
		if _, err := NewSanitizer(journal.NewRegistryScrubber())("application/x-go-trace", malformed); err == nil {
			t.Fatal("malformed trace accepted")
		}
	}
}
