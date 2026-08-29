package dispatcher

import (
	"errors"
	"strings"
	"testing"
)

const (
	fullSha  = "0123456789abcdef0123456789abcdef01234567"
	otherSha = "fedcba9876543210fedcba9876543210fedcba98"
)

// §8 item 7, the NEGATIVE case (assertable today): an image whose sha tag
// does not equal the dispatcher's embedded commit is REFUSED with a NAMED
// diagnostic. The positive case — trusting a matching tag — is gated on the
// unbuilt publish-side sha-tag stamp gate (decision 009, dispatcher §6/§9)
// and is deliberately not asserted as proof of anything here.
func TestVerifySkewRefusesMismatchedShaTag(t *testing.T) {
	err := VerifySkew(fullSha, "v0.1.0", "ghcr.io/goobers/goobers-base:"+otherSha)
	var skew *SkewError
	if !errors.As(err, &skew) {
		t.Fatalf("got err %v, want SkewError", err)
	}
	for _, needle := range []string{"version-skew", otherSha, fullSha, "#1061"} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("diagnostic %q does not name %q", err.Error(), needle)
		}
	}
}

func TestVerifySkewAcceptsMatchingShaTag(t *testing.T) {
	if err := VerifySkew(fullSha, "v0.1.0", "ghcr.io/goobers/goobers-base:"+fullSha); err != nil {
		t.Fatalf("matching full sha refused: %v", err)
	}
	// The Makefile stamps the SHORT sha; prefix equality of the same commit
	// must pass.
	if err := VerifySkew(fullSha[:12], "v0.1.0", "ghcr.io/goobers/goobers-base:"+fullSha); err != nil {
		t.Fatalf("matching short-sha stamp refused: %v", err)
	}
	// A short stamp that is NOT a prefix still refuses.
	if err := VerifySkew("deadbeef4444", "v0.1.0", "ghcr.io/goobers/goobers-base:"+fullSha); err == nil {
		t.Fatal("non-prefix short stamp accepted")
	}
}

func TestVerifySkewRefusesUnverifiableReferences(t *testing.T) {
	cases := map[string]string{
		"no tag":       "ghcr.io/goobers/goobers-base",
		"latest":       "ghcr.io/goobers/goobers-base:latest",
		"foreign tag":  "ghcr.io/goobers/goobers-base:nightly",
		"digest only":  "ghcr.io/goobers/goobers-base@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"port no tag":  "registry.example.com:5000/goobers/goobers-base",
		"wrong length": "ghcr.io/goobers/goobers-base:" + fullSha[:39],
	}
	for name, image := range cases {
		var skew *SkewError
		if err := VerifySkew(fullSha, "v0.1.0", image); !errors.As(err, &skew) {
			t.Errorf("%s (%q): got %v, want SkewError", name, image, err)
		}
	}
}

// An UNSTAMPED dispatcher cannot verify skew and refuses closed, naming its
// own missing stamp — not the image.
func TestVerifySkewRefusesWhenDispatcherUnstamped(t *testing.T) {
	for _, commit := range []string{"", "none"} {
		err := VerifySkew(commit, "dev", "ghcr.io/goobers/goobers-base:"+fullSha)
		var skew *SkewError
		if !errors.As(err, &skew) {
			t.Fatalf("embedded commit %q: got %v, want SkewError", commit, err)
		}
		if !strings.Contains(err.Error(), "no embedded commit stamp") {
			t.Errorf("diagnostic should name the dispatcher's own missing stamp, got %q", err.Error())
		}
	}
}

// The DI-6 release-time reading: on a tagged release, tag = version.
func TestVerifySkewReleaseTagEqualsVersion(t *testing.T) {
	if err := VerifySkew(fullSha, "v0.1.0", "ghcr.io/goobers/goobers-base:v0.1.0"); err != nil {
		t.Fatalf("release tag equal to embedded version refused: %v", err)
	}
	if err := VerifySkew(fullSha, "v0.1.0", "ghcr.io/goobers/goobers-base:v0.2.0"); err == nil {
		t.Fatal("release tag differing from embedded version accepted")
	}
	// A "dev" version never matches a release tag.
	if err := VerifySkew(fullSha, "dev", "ghcr.io/goobers/goobers-base:dev"); err == nil {
		t.Fatal(`a "dev" version must not satisfy the release-tag comparison`)
	}
}

// The tag parser must not mistake a registry port for a tag.
func TestImageTagParsing(t *testing.T) {
	cases := map[string]string{
		"registry.example.com:5000/goobers/base:" + fullSha: fullSha,
		"registry.example.com:5000/goobers/base":            "",
		"goobers/base:v1@sha256:" + strings.Repeat("a", 64): "v1",
		"goobers/base": "",
	}
	for image, want := range cases {
		if got := imageTag(image); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", image, got, want)
		}
	}
}
