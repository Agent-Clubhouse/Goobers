package instance

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestBlobReaderBatchesBlobsAndRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("", "init", "-q")
	payload := "a\x00b\r\n\n"
	first := run(payload, "hash-object", "-w", "--stdin")
	empty := run("", "hash-object", "-w", "--stdin")

	r := newBlobReader(context.Background(), &GitSource{}, []string{"-C", dir})
	defer r.close()
	for _, tc := range []struct{ id, want string }{{first, payload}, {empty, ""}, {first, payload}} {
		var buf bytes.Buffer
		if err := r.read(tc.id, &buf); err != nil {
			t.Fatalf("read %s: %v", tc.id, err)
		}
		if buf.String() != tc.want {
			t.Fatalf("read %s = %q, want %q", tc.id, buf.String(), tc.want)
		}
	}
	if err := r.read(strings.Repeat("0", 40), &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for missing object")
	}
	if err := r.read(first, &bytes.Buffer{}); err == nil {
		t.Fatal("expected reader to stay failed after error")
	}
}
