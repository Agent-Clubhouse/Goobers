package artifactset

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestSanitizeJSONDecodesSecretsAndPreservesNumbers(t *testing.T) {
	s := journal.NewRegistryScrubber()
	s.Register([]byte("test-secret-material"))
	input := []byte(`{"nested":[{"value":"test-\u0073ecret-material"}],"counter":9007199254740993,"decimal":1.234567890123456789}`)
	clean, err := NewSanitizer(s)("application/json", input)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Nested  []struct{ Value string }
		Counter json.Number
		Decimal json.Number
	}
	if err := json.Unmarshal(clean, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Nested) != 1 || got.Nested[0].Value != journal.Redacted || got.Counter.String() != "9007199254740993" || got.Decimal.String() != "1.234567890123456789" {
		t.Fatalf("redaction or numeric fidelity lost: %s", clean)
	}
	for _, invalid := range []string{
		`{"test-\u0073ecret-material":"value"}`,
		`{"value":"first","v\u0061lue":"second"}`,
		strings.Repeat("[", 18) + "0" + strings.Repeat("]", 18),
	} {
		if _, err := NewSanitizer(s)("application/json", []byte(invalid)); err == nil {
			t.Fatal("unsafe JSON accepted")
		}
	}
}

func archiveFixture(t *testing.T, kind byte, name, content string) []byte {
	t.Helper()
	var output bytes.Buffer
	w := tar.NewWriter(&output)
	h := &tar.Header{Name: name, Typeflag: kind, Mode: 0o755, Uname: "private-host-user", Gname: "private-host-group", Size: int64(len(content))}
	if kind != tar.TypeReg {
		h.Size = 0
		content = ""
	}
	if err := w.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestSanitizeArchiveRedactsBeforeRebuilding(t *testing.T) {
	s := journal.NewRegistryScrubber()
	s.Register([]byte("test-secret-material"))
	sanitize := NewSanitizer(s)
	raw := archiveFixture(t, tar.TypeReg, "repro.sh", "#!/bin/sh\necho test-secret-material\n")
	for _, compressed := range []bool{false, true} {
		input, media := raw, "application/x-tar"
		if compressed {
			var output bytes.Buffer
			w := gzip.NewWriter(&output)
			w.Name = "private-host-name"
			if _, err := w.Write(raw); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			input, media = output.Bytes(), "application/gzip"
		}
		clean, err := sanitize(media, input)
		if err != nil {
			t.Fatal(err)
		}
		again, err := sanitize(media, input)
		if err != nil || !bytes.Equal(clean, again) {
			t.Fatalf("nondeterministic: %v", err)
		}
		if compressed {
			r, err := gzip.NewReader(bytes.NewReader(clean))
			if err != nil {
				t.Fatal(err)
			}
			if r.Name != "" {
				t.Fatal("gzip leaked metadata")
			}
			clean, err = io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
		}
		r := tar.NewReader(bytes.NewReader(clean))
		h, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if h.Uname != "" || h.Gname != "" || h.Mode != 0o755 {
			t.Fatalf("metadata or executable mode: %+v", h)
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("test-secret-material")) || !bytes.Contains(data, []byte(journal.Redacted)) {
			t.Fatalf("unredacted payload: %q", data)
		}
	}
}

func TestSanitizeRejectsUnsafePayloads(t *testing.T) {
	s := NewSanitizer(journal.NewRegistryScrubber())
	for _, data := range [][]byte{
		archiveFixture(t, tar.TypeSymlink, "link", ""),
		archiveFixture(t, tar.TypeReg, "../escape", "text"),
		archiveFixture(t, tar.TypeReg, "binary", "\x00\x01\xff"),
	} {
		if _, err := s("application/x-tar", data); err == nil {
			t.Fatal("unsafe archive accepted")
		}
	}
	if _, err := s("application/octet-stream", []byte("opaque")); err == nil {
		t.Fatal("opaque format accepted")
	}
	if _, err := s("application/json", []byte("not JSON")); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}
