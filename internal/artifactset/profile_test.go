package artifactset

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"runtime/pprof"
	"testing"

	"github.com/google/pprof/profile"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/goobers/goobers/internal/journal"
)

func compressedProfileFixture(t *testing.T, raw []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	w := gzip.NewWriter(&output)
	w.Name = "private-host-metadata"
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestSanitizeNativeProfile(t *testing.T) {
	var captured bytes.Buffer
	pprof.Do(context.Background(), pprof.Labels("credential", "private-profile-credential"), func(context.Context) {
		if err := pprof.Lookup("goroutine").WriteTo(&captured, 0); err != nil {
			t.Fatal(err)
		}
	})
	z, err := gzip.NewReader(bytes.NewReader(captured.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("private-profile-credential")) {
		t.Fatal("profile fixture lacks test credential")
	}
	input := compressedProfileFixture(t, raw)
	scrubber := journal.NewRegistryScrubber()
	sanitize := NewSanitizer(scrubber)
	clean, err := sanitize("application/x-pprof", input)
	if err != nil {
		t.Fatal(err)
	}
	p, err := profile.ParseData(clean)
	if err != nil || p.CheckValid() != nil || len(p.Sample) == 0 {
		t.Fatalf("sanitized profile is not usable: %v", err)
	}
	decoded, err := gzip.NewReader(bytes.NewReader(clean))
	if err != nil {
		t.Fatal(err)
	}
	preserved, err := io.ReadAll(decoded)
	if err != nil || !bytes.Equal(preserved, raw) || decoded.Name != "" {
		t.Fatalf("profile changed or gzip metadata leaked: %v", err)
	}
	if err := decoded.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := sanitize("application/x-pprof", input)
	if err != nil || !bytes.Equal(clean, again) {
		t.Fatalf("profile normalization is nondeterministic: %v", err)
	}
	scrubber.Register([]byte("private-profile-credential"))
	if _, err := sanitize("application/x-pprof", input); err == nil || bytes.Contains([]byte(err.Error()), []byte("private-profile-credential")) {
		t.Fatalf("unsafe profile accepted or leaked in error: %v", err)
	}
	unknown := protowire.AppendTag(bytes.Clone(raw), 100, protowire.BytesType)
	unknown = protowire.AppendBytes(unknown, []byte("opaque"))
	for _, invalid := range [][]byte{
		[]byte("not a profile"),
		input[:len(input)/2],
		append(bytes.Clone(input), input...),
		compressedProfileFixture(t, unknown),
		compressedProfileFixture(t, []byte{0x32, 0}),
		compressedProfileFixture(t, bytes.Repeat([]byte{0}, MaxPayloadBytes+1)),
	} {
		if _, err := NewSanitizer(journal.NewRegistryScrubber())("application/x-pprof", invalid); err == nil {
			t.Fatal("unsafe profile accepted")
		}
	}
}

func TestProfileWireAllocationBudget(t *testing.T) {
	for _, raw := range [][]byte{
		bytes.Repeat([]byte{0x0a, 0}, 20), // Repeated empty sample-type messages.
		protowire.AppendBytes([]byte{0x12}, protowire.AppendBytes([]byte{0x0a}, bytes.Repeat([]byte{0}, 20))), // Packed locations.
	} {
		budget := 10
		if err := checkProfileWire(raw, "profile", &budget); err == nil {
			t.Fatal("allocation budget not enforced before decoding")
		}
	}
}
