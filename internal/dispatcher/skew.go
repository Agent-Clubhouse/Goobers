package dispatcher

import (
	"fmt"
	"strings"
)

// SkewError is the decision-009 version-skew refusal: the dispatcher launches
// stage pods only from an image whose tag encodes its own embedded commit
// (sha-tag channel) or its own embedded version (release channel). The
// diagnostic is NAMED — "version-skew refusal" plus both sides of the
// comparison — because the recorded #1061 failure class is skew that reads as
// something else.
//
// Soundness caveat, stated not assumed (dispatcher §6): a MATCHING tag proves
// nothing until the publish-side stamp gate covering the continuous-main
// sha-tag channel exists (decision 009 prerequisite, unbuilt — dispatcher §9).
// This check implements the refusal (the negative case, assertable today);
// tag-trust on the positive case is only as sound as the build discipline
// behind the tag.
type SkewError struct {
	// Image is the refused image reference.
	Image string
	// Reason names why the comparison refused.
	Reason string
}

func (e *SkewError) Error() string {
	return fmt.Sprintf("dispatcher: version-skew refusal for image %q: %s (decision 009: the stage image must carry this dispatcher's commit; no registry read — the tag IS the comparison)", e.Image, e.Reason)
}

// VerifySkew compares the dispatcher's embedded commit sha (and, for
// release-tagged images, its embedded version) to the image reference's tag —
// a TAG comparison, never a registry read (decision 009 / DI-6): on the
// continuous-main channel images are sha-tagged (goobers-base:<40-char-sha>)
// and tag equality IS commit equality; on a tagged release, tag = version =
// commit stamp. Everything unverifiable refuses closed with a named
// diagnostic: no tag, a "latest" tag (#3452), an unstamped dispatcher binary,
// or a tag that is neither the commit nor the version.
func VerifySkew(embeddedCommit, embeddedVersion, image string) error {
	tag := imageTag(image)
	switch tag {
	case "":
		return &SkewError{Image: image, Reason: "the reference carries no tag, so skew is unverifiable — sha-tag the image (goobers-base:<40-char-sha>)"}
	case "latest":
		return &SkewError{Image: image, Reason: `the "latest" tag encodes no commit and is banned on the sha-tag channel (#3452)`}
	}

	if isCommitSha(tag) {
		if !commitStamped(embeddedCommit) {
			return &SkewError{Image: image, Reason: fmt.Sprintf(
				"the dispatcher binary itself carries no embedded commit stamp (%q) to compare tag %q against — an unstamped dispatcher cannot verify skew and refuses closed", embeddedCommit, tag)}
		}
		if !commitsEqual(embeddedCommit, tag) {
			return &SkewError{Image: image, Reason: fmt.Sprintf(
				"image tag sha %q does not equal the dispatcher's embedded commit %q — daemon/dispatcher and image are from different builds (#1061 skew class)", tag, embeddedCommit)}
		}
		return nil
	}

	if embeddedVersion != "" && embeddedVersion != "dev" && tag == embeddedVersion {
		return nil
	}
	return &SkewError{Image: image, Reason: fmt.Sprintf(
		"image tag %q is neither a 40-char commit sha equal to the dispatcher's embedded commit nor the dispatcher's release version %q", tag, embeddedVersion)}
}

// imageTag extracts the tag from an image reference ("" when absent). A
// digest suffix (@sha256:…) is not a tag: the tag is what precedes it.
func imageTag(image string) string {
	ref := image
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash {
		return ""
	}
	return ref[colon+1:]
}

// isCommitSha reports whether tag is a 40-character hex commit sha.
func isCommitSha(tag string) bool {
	if len(tag) != 40 {
		return false
	}
	for _, r := range tag {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// commitStamped reports whether an embedded commit value is a real stamp
// (internal/version defaults to "none" on an unstamped build).
func commitStamped(commit string) bool {
	return commit != "" && commit != "none" && len(commit) >= 7
}

// commitsEqual compares an embedded commit stamp to a 40-char tag sha. The
// Makefile stamps the SHORT sha, so equality is prefix equality with the
// shorter side at least 7 characters — an abbreviation of the same commit,
// never a different one.
func commitsEqual(embedded, tag string) bool {
	a := strings.ToLower(embedded)
	b := strings.ToLower(tag)
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 7 && strings.HasPrefix(b, a)
}
