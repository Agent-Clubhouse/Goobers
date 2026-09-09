package secretpattern

import (
	"bytes"
	"strings"
	"testing"
)

func TestCheckpointBoundaryEverySecretSplit(t *testing.T) {
	secrets := []string{
		"ghp_" + strings.Repeat("A", 80),
		"github_pat_" + strings.Repeat("a", 80),
		"AKIA" + strings.Repeat("Z", 16),
		"xoxb-" + strings.Repeat("a", 80),
		"sk-ant-" + strings.Repeat("a", 80),
		"glpat-" + strings.Repeat("a", 80),
		"eyJabc.def.ghi",
		"-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("a", 200) + "\n-----END RSA PRIVATE KEY-----",
		"bEaReR \n" + strings.Repeat("a", 80) + "==",
		"BASIC " + strings.Repeat("a", 80) + "==",
	}
	s := NewScrubber()
	for _, secret := range secrets {
		input := []byte("ordinary output: " + secret + "\nfinished.\n")
		want := s.Scrub(input)
		if bytes.Equal(want, input) {
			t.Fatal("fixture did not contain a recognized secret")
		}
		for split := range len(input) + 1 {
			boundary := s.SafePrefix(input[:split])
			got := append(bytes.Clone(s.Scrub(input[:boundary])), s.Scrub(input[boundary:])...)
			if !bytes.Equal(got, want) {
				t.Fatalf("unsafe checkpoint: fixture=%d split=%d boundary=%d", len(secret), split, boundary)
			}
		}
	}
}

func TestCheckpointBoundaryRetainsIncompletePEMAndUTF8(t *testing.T) {
	s := NewScrubber()
	prefix := "output: "
	input := []byte(prefix + "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("A", 10000))
	if got := s.SafePrefix(input); got != len(prefix) {
		t.Fatalf("unfinished PEM boundary=%d want=%d", got, len(prefix))
	}
	input = append([]byte(prefix), 0xe2, 0x82)
	if got := s.SafePrefix(input); got != len(prefix) {
		t.Fatalf("partial UTF-8 boundary=%d", got)
	}
	if got := s.SafePrefix([]byte("ordinary output.\n")); got != len("ordinary output.\n") {
		t.Fatalf("ordinary output retained: boundary=%d", got)
	}
}
