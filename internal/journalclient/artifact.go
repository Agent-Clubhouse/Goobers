package journalclient

import (
	"errors"
	"fmt"
	"mime"
	"path"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// artifact.go is the TYPED ARTIFACT-CONTENT FETCH on the C4 run-scoped
// journal plane (Goobers#3996 blocker 2, extending #3880's read route set and
// composing it with #3634's content-addressed blob discipline).
//
// The reads #3880 converted want journal EVENTS. One reader wants the body
// behind an event: file-issues confirms a nomination's evidence against the
// deterministic signals stage's own stdout, byte for byte (decision 004), and
// that stdout is an artifact, not an event field. Handing a stage a general
// "fetch me these bytes" call is the thing decision 005 R1 declined to
// expose, so this is deliberately NOT that. It is one narrow question:
//
//	"give me the content of the artifact THIS RUN'S OWN JOURNAL records for
//	 stage S under a name ending in N"
//
// and every part of that sentence is load-bearing:
//
//   - THIS RUN'S: the reader is already contained to the caller's own run —
//     the File backend by the run directory it opened, the HTTP backend by the
//     journal-scoped bearer the daemon issued for exactly one run, refused by
//     podRunContained on the server and by the client's own runPath, which
//     never takes a run from a caller. Nothing here widens that, and there is
//     no cross-run or cross-gaggle arm: the three gaggle-scoped questions on
//     CrossRun stay the only reads that leave a run.
//   - OWN JOURNAL RECORDS: the reference is RESOLVED from the run's event log,
//     never supplied by the caller. A caller cannot name a filesystem path, a
//     host path, or even a digest; it names a stage and a recorded artifact
//     name, and if the journal has no such record the answer is an explicit
//     refusal (ErrArtifactNotRecorded) and never a success-shaped empty body.
//   - CONTENT: the bytes are verified against the digest the journal recorded
//     for them, on both backends, after being bounded by size and media type
//     BEFORE they are fetched.
//
// The read is steered by DIGEST, not by the path the journal happens to
// carry. A recorded Ref.Path that is not exactly the canonical
// content-addressed path for its own digest is refused outright
// (ErrArtifactUnsafeRef) rather than repaired, so a tampered run.yaml or a
// hand-edited event log cannot point this read at ../../ anything — belt and
// braces over journal.Reader's own containment guard, and the only check that
// exists at all on the plane, where no path crosses the boundary.

// Errors this fetch answers with. Every one is explicit: a stage that cannot
// read the artifact it was going to decide on must say which of these
// happened, not proceed as though the artifact was empty.
var (
	// ErrArtifactNotRecorded reports that the run's own journal records no
	// successful traversal of the named stage carrying a matching artifact.
	// It is the ONLY "there is nothing here" answer, and it is still an
	// error — an absent artifact and an unreadable one are different facts
	// and neither is an empty body.
	ErrArtifactNotRecorded = errors.New("journalclient: this run's journal records no such stage artifact")
	// ErrArtifactUnsafeRef reports a recorded reference that does not address
	// the canonical content-addressed path for its own digest — a malformed
	// digest, or a path that has been steered somewhere else.
	ErrArtifactUnsafeRef = errors.New("journalclient: the recorded artifact reference does not address a canonical journal artifact path")
	// ErrArtifactTooLarge reports an artifact outside the caller's size bound,
	// refused before its bytes are fetched where the journal recorded a size,
	// and again after, where the served body disagreed with it.
	ErrArtifactTooLarge = errors.New("journalclient: the recorded artifact exceeds the caller's size bound")
	// ErrArtifactMediaType reports an artifact whose recorded media type is
	// outside the caller's bound.
	ErrArtifactMediaType = errors.New("journalclient: the recorded artifact's media type is outside the caller's bound")
	// ErrArtifactIntegrity reports content that did not match what the journal
	// recorded about it — a digest mismatch, or a length the recorded size
	// contradicts.
	ErrArtifactIntegrity = errors.New("journalclient: the artifact's content does not match what the run journal recorded")
)

// MaxStageArtifactBytes is the hard ceiling on one typed artifact fetch,
// applied whatever a caller asks for. A stage-side reader is a pod with a
// small memory limit; a caller that names no bound, or names a larger one,
// gets this.
const MaxStageArtifactBytes = 16 << 20

// GenericMediaType is what the daemon's read projection normalizes an
// unrecorded media type to (readservice.normalizeMediaType). It is the reason
// MediaTypes admits an unspecific declaration — see ArtifactBounds.
const GenericMediaType = "application/octet-stream"

// ArtifactContent is one journal artifact's verified bytes together with the
// typed facts the run's own journal recorded about it. Nothing here is
// caller-supplied and nothing here is a host path: an artifact is identified
// by the stage that produced it, the name it was recorded under, and its
// content address.
type ArtifactContent struct {
	// Stage is the stage whose successful finish declared the artifact.
	Stage string
	// Name is the name the artifact was recorded under.
	Name string
	// Digest is the content address the bytes were verified against.
	Digest string
	// Size is the length of Bytes, equal to the size the journal recorded.
	Size int64
	// MediaType is the artifact's declared media type, normalized;
	// GenericMediaType when the journal recorded none.
	MediaType string
	// Bytes is the verified content.
	Bytes []byte
}

// ArtifactBounds bounds one typed fetch. Both bounds are the CALLER's
// declaration of what it is willing to act on, checked against what the
// journal recorded before any content is fetched.
type ArtifactBounds struct {
	// MaxBytes is the caller's ceiling. Zero or negative means
	// MaxStageArtifactBytes, which also caps anything larger.
	MaxBytes int64
	// MediaTypes is the media types the caller will act on. Empty admits any.
	//
	// A GENERIC OR ABSENT declaration is admitted whatever this says, and that
	// is a deliberate parity property rather than a hole. The journal's
	// artifact.recorded events carry no media type at all (journal.Run's
	// recordArtifact derives Path/Digest/Size and nothing else), and the
	// daemon's scrubbed projection normalizes that absence to
	// GenericMediaType. So requiring a SPECIFIC type here would refuse on the
	// plane exactly what it admits on disk — a stage would approve on a
	// daemon host and refuse in a pod, which is the silent-divergence class
	// this whole seam exists to close. What the bound does enforce is the
	// case that carries real information: an artifact that POSITIVELY
	// declares a type outside the list is refused on both backends.
	MediaTypes []string
}

func (b ArtifactBounds) limit() int64 {
	if b.MaxBytes <= 0 || b.MaxBytes > MaxStageArtifactBytes {
		return MaxStageArtifactBytes
	}
	return b.MaxBytes
}

// admits reports whether mediaType is one the caller will act on.
func (b ArtifactBounds) admits(mediaType string) bool {
	if len(b.MediaTypes) == 0 || mediaType == "" || mediaType == GenericMediaType {
		return true
	}
	for _, allowed := range b.MediaTypes {
		if normalizeMediaType(allowed) == mediaType {
			return true
		}
	}
	return false
}

// normalizeMediaType restates readservice's normalization so the two backends
// compare the same spelling of one type. An unparseable or absent declaration
// is the generic type, exactly as the daemon's projection reports it.
func normalizeMediaType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return GenericMediaType
	}
	parsed, params, err := mime.ParseMediaType(value)
	if err != nil {
		return GenericMediaType
	}
	return mime.FormatMediaType(parsed, params)
}

// StageArtifactRef is a reference the run's own journal records, resolved from
// its event log rather than named by a caller.
type StageArtifactRef struct {
	// Ref is the artifact.recorded reference: the content address plus the
	// size and journal-relative path recorded with it. On the plane the path
	// is derived from the digest, never carried across the boundary.
	Ref journal.Ref
	// Stage is the stage whose successful finish declared it.
	Stage string
	// Name is the name it was recorded under.
	Name string
	// MediaType is the most specific declaration available: the one the
	// finishing stage declared for the pointer, falling back to the one
	// recorded with the artifact. Normalized.
	MediaType string
}

// ResolveStageArtifact finds the artifact a run's journal records for stage's
// NEWEST SUCCESSFUL finish under a name ending in nameSuffix.
//
// It is the one spelling of that rule: an artifact only counts when BOTH an
// artifact.recorded event carries it under a matching name AND a successful
// stage.finished event for that stage declares its digest. Neither half alone
// binds — a stage that failed declares nothing, and an artifact recorded by
// some other stage is not this stage's output — which is what makes the
// result something a caller could not have steered.
func ResolveStageArtifact(events []journal.Event, stage, nameSuffix string) (StageArtifactRef, bool) {
	type recorded struct {
		ref  journal.Ref
		name string
	}
	artifacts := make(map[string]recorded)
	for _, event := range events {
		if event.Type == journal.EventArtifactRecorded && event.Ref != nil && strings.HasSuffix(event.Name, nameSuffix) {
			artifacts[event.Ref.Digest] = recorded{ref: *event.Ref, name: event.Name}
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventStageFinished || event.Stage != stage || event.Status != string(apiv1.ResultSuccess) {
			continue
		}
		for _, declared := range event.Artifacts {
			artifact, ok := artifacts[declared.Digest]
			if !ok {
				continue
			}
			mediaType := normalizeMediaType(declared.MediaType)
			if mediaType == GenericMediaType {
				mediaType = normalizeMediaType(artifact.ref.MediaType)
			}
			return StageArtifactRef{
				Ref:       artifact.ref,
				Stage:     stage,
				Name:      artifact.name,
				MediaType: mediaType,
			}, true
		}
	}
	return StageArtifactRef{}, false
}

// StageArtifactContent is the typed fetch itself: resolve the reference from
// the caller's OWN run journal, bound it, fetch it through whichever backend
// the caller is on, and verify the bytes against what the journal recorded.
//
// reader is already contained to one run, so there is no run parameter and no
// way to ask for another one. Every failure is an error; there is no
// success-shaped empty answer for any of them.
func StageArtifactContent(reader Reader, stage, nameSuffix string, bounds ArtifactBounds) (ArtifactContent, error) {
	if reader == nil {
		return ArtifactContent{}, errors.New("journalclient: a run journal reader is required")
	}
	if strings.TrimSpace(stage) == "" {
		return ArtifactContent{}, errors.New("journalclient: stage is required")
	}
	if strings.TrimSpace(nameSuffix) == "" {
		return ArtifactContent{}, errors.New("journalclient: an artifact name suffix is required")
	}
	events, err := reader.Events()
	if err != nil {
		return ArtifactContent{}, err
	}
	resolved, ok := ResolveStageArtifact(events, stage, nameSuffix)
	if !ok {
		return ArtifactContent{}, fmt.Errorf("%w: run %s, stage %q, name ending %q",
			ErrArtifactNotRecorded, reader.RunID(), stage, nameSuffix)
	}
	return fetchResolvedArtifact(reader, resolved, bounds)
}

func fetchResolvedArtifact(reader Reader, resolved StageArtifactRef, bounds ArtifactBounds) (ArtifactContent, error) {
	// Address the read by digest alone. A recorded path that is not the
	// canonical one for that digest is a tampered or foreign reference, and it
	// is refused rather than silently corrected: nothing legitimate writes one.
	canonical, err := journal.ArtifactPath(resolved.Ref.Digest)
	if err != nil {
		return ArtifactContent{}, fmt.Errorf("%w: %s: %w", ErrArtifactUnsafeRef, resolved.Name, err)
	}
	if recorded := resolved.Ref.Path; recorded != "" && path.Clean(strings.ReplaceAll(recorded, "\\", "/")) != canonical {
		return ArtifactContent{}, fmt.Errorf("%w: %s records path %q, want %q",
			ErrArtifactUnsafeRef, resolved.Name, recorded, canonical)
	}

	limit := bounds.limit()
	if resolved.Ref.Size < 0 {
		return ArtifactContent{}, fmt.Errorf("%w: %s records a negative size %d",
			ErrArtifactUnsafeRef, resolved.Name, resolved.Ref.Size)
	}
	if resolved.Ref.Size > limit {
		return ArtifactContent{}, fmt.Errorf("%w: %s is %d bytes, over the %d byte bound",
			ErrArtifactTooLarge, resolved.Name, resolved.Ref.Size, limit)
	}
	if !bounds.admits(resolved.MediaType) {
		return ArtifactContent{}, fmt.Errorf("%w: %s declares %s, want one of %s",
			ErrArtifactMediaType, resolved.Name, resolved.MediaType, strings.Join(bounds.MediaTypes, ", "))
	}

	data, err := reader.ArtifactBytesBounded(journal.Ref{
		Path:      canonical,
		Digest:    resolved.Ref.Digest,
		Size:      resolved.Ref.Size,
		MediaType: resolved.Ref.MediaType,
		Integrity: resolved.Ref.Integrity,
	}, limit)
	if err != nil {
		return ArtifactContent{}, fmt.Errorf("read %s of stage %q: %w", resolved.Name, resolved.Stage, err)
	}
	// Both backends verify the digest already. Re-verifying here keeps the
	// guarantee attached to the TYPED fetch rather than to whichever backend
	// answered it, so a future backend cannot quietly serve unverified bytes.
	if got := journal.Digest(data); got != resolved.Ref.Digest {
		return ArtifactContent{}, fmt.Errorf("%w: %s digests %s, want %s",
			ErrArtifactIntegrity, resolved.Name, got, resolved.Ref.Digest)
	}
	if int64(len(data)) > limit {
		return ArtifactContent{}, fmt.Errorf("%w: %s served %d bytes, over the %d byte bound",
			ErrArtifactTooLarge, resolved.Name, len(data), limit)
	}
	if int64(len(data)) != resolved.Ref.Size {
		return ArtifactContent{}, fmt.Errorf("%w: %s served %d bytes, but the journal recorded %d",
			ErrArtifactIntegrity, resolved.Name, len(data), resolved.Ref.Size)
	}
	return ArtifactContent{
		Stage:     resolved.Stage,
		Name:      resolved.Name,
		Digest:    resolved.Ref.Digest,
		Size:      int64(len(data)),
		MediaType: resolved.MediaType,
		Bytes:     data,
	}, nil
}
