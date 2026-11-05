package fanout

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"sync"

	apischemas "github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/jcs"
	"github.com/goobers/goobers/internal/journal"
)

// MaxFrozenItemBytes keeps the activity result plus the next dispatch input's
// second copy of one canonical item below Temporal's 2 MiB payload limit with
// roughly 512 KiB left for envelopes and activity metadata.
const MaxFrozenItemBytes = 768 << 10

// Host bounds on a roster source (design §6.1). Raising them is a host
// capacity change; the wire shape and validation semantics stay the same.
const (
	// MaxSourceBytes equals workflow.MaxFanOutSourceBytes: the size bound is
	// checked before any decoding.
	MaxSourceBytes int64 = 1 << 20
	// MaxRosterItems equals the static parallel width bound.
	MaxRosterItems = 32
	// SourceMediaType is the only media type a fan-out source may have.
	SourceMediaType = "application/json"
)

var (
	identityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ValidIdentity reports whether identity matches the roster identity charset.
func ValidIdentity(identity string) bool { return identityPattern.MatchString(identity) }

// ValidDigest reports whether digest is "sha256:" plus 64 lowercase hex digits.
func ValidDigest(digest string) bool { return digestPattern.MatchString(digest) }

// ItemDigest is the canonical item digest: "sha256:" plus the lowercase hex
// SHA-256 of the RFC 8785 serialization of {"identity": identity, "item":
// item}. A roster entry's own digest field is deliberately outside the hashed
// value, which is what makes verifying a supplied digest possible at all.
func ItemDigest(identity string, item json.RawMessage) (string, error) {
	if !isJSONObject(item) {
		return "", fmt.Errorf("item is not a JSON object")
	}
	canonical, err := jcs.Marshal(struct {
		Identity string          `json:"identity"`
		Item     json.RawMessage `json:"item"`
	}{identity, item})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type rosterEnvelope struct {
	SchemaVersion int           `json:"schemaVersion"`
	Items         []rosterEntry `json:"items"`
}

type rosterEntry struct {
	Identity string          `json:"identity"`
	Item     json.RawMessage `json:"item"`
	Digest   *string         `json:"digest"`
}

// FreezeRoster applies the roster freezing checks of design §4.2 steps 2-3
// and §6.2 steps 1-3 to one source document: the media type, the byte size
// (before decoding), the closed envelope (api/schemas/fanout-roster.schema.json),
// the identity charset, duplicate identities, and every digest. It returns
// the normalized roster: items sorted by identity in bytewise UTF-8 order,
// each with its canonical item bytes and computed digest. An empty roster is
// valid and returns an empty, non-nil slice.
//
// Every failure is a *SourceError (fanout_source_invalid). FreezeRoster never
// partially succeeds.
func FreezeRoster(data []byte, mediaType string) ([]journal.MapItem, error) {
	if mediaType != SourceMediaType {
		return nil, &SourceError{Reason: ReasonMediaType, Detail: fmt.Sprintf("media type %q, want %q", mediaType, SourceMediaType)}
	}
	if int64(len(data)) > MaxSourceBytes {
		return nil, &SourceError{Reason: ReasonTooLarge, Detail: fmt.Sprintf("%d bytes exceeds the %d byte bound", len(data), MaxSourceBytes)}
	}
	schema, err := rosterEnvelopeSchema()
	if err != nil {
		return nil, err
	}
	if verdict := schema.Check(data); !verdict.Valid {
		return nil, &SourceError{Reason: ReasonEnvelope, Detail: firstIssue(verdict)}
	}
	var envelope rosterEnvelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, &SourceError{Reason: ReasonEnvelope, Detail: err.Error()}
	}
	if len(envelope.Items) > MaxRosterItems {
		return nil, &SourceError{Reason: ReasonEnvelope, Detail: fmt.Sprintf("%d items exceeds the %d item bound", len(envelope.Items), MaxRosterItems)}
	}
	items := make([]journal.MapItem, 0, len(envelope.Items))
	seen := make(map[string]bool, len(envelope.Items))
	for _, entry := range envelope.Items {
		item, err := freezeEntry(entry, seen)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	// Bytewise UTF-8 order: Go string comparison compares bytes.
	sort.Slice(items, func(i, j int) bool { return items[i].Identity < items[j].Identity })
	return items, nil
}

func freezeEntry(entry rosterEntry, seen map[string]bool) (journal.MapItem, error) {
	if !ValidIdentity(entry.Identity) {
		return journal.MapItem{}, &SourceError{Reason: ReasonIdentity, Identity: entry.Identity, Detail: "identity must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"}
	}
	if seen[entry.Identity] {
		return journal.MapItem{}, &SourceError{Reason: ReasonDuplicateIdentity, Identity: entry.Identity, Detail: "identity appears more than once"}
	}
	seen[entry.Identity] = true
	canonical, err := jcs.Canonicalize(entry.Item)
	if err != nil || !isJSONObject(canonical) {
		return journal.MapItem{}, &SourceError{Reason: ReasonItem, Identity: entry.Identity, Detail: "item must be a canonicalizable JSON object"}
	}
	if len(canonical) > MaxFrozenItemBytes {
		return journal.MapItem{}, &SourceError{
			Reason: ReasonTooLarge, Identity: entry.Identity,
			Detail: fmt.Sprintf("canonical item is %d bytes, bound is %d", len(canonical), MaxFrozenItemBytes),
		}
	}
	digest, err := ItemDigest(entry.Identity, canonical)
	if err != nil {
		return journal.MapItem{}, &SourceError{Reason: ReasonItem, Identity: entry.Identity, Detail: err.Error()}
	}
	if entry.Digest != nil {
		if !ValidDigest(*entry.Digest) {
			return journal.MapItem{}, &SourceError{Reason: ReasonMalformedDigest, Identity: entry.Identity, Detail: "digest must be sha256: plus 64 lowercase hex digits"}
		}
		if *entry.Digest != digest {
			return journal.MapItem{}, &SourceError{Reason: ReasonDigestMismatch, Identity: entry.Identity,
				Detail: fmt.Sprintf("supplied digest %s, canonical digest of {identity, item} is %s", *entry.Digest, digest)}
		}
	}
	return journal.MapItem{Identity: entry.Identity, Digest: digest, Item: canonical}, nil
}

func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func firstIssue(verdict handoffcheck.Verdict) string {
	if len(verdict.Issues) == 0 {
		return "document does not match the roster envelope"
	}
	issue := verdict.Issues[0]
	if issue.Path != "" {
		return fmt.Sprintf("%s at %s: %s", issue.Code, issue.Path, issue.Message)
	}
	return fmt.Sprintf("%s: %s", issue.Code, issue.Message)
}

var (
	rosterSchemaOnce sync.Once
	rosterSchema     *handoffcheck.Schema
	rosterSchemaErr  error
)

// rosterEnvelopeSchema compiles the embedded host envelope once per process.
func rosterEnvelopeSchema() (*handoffcheck.Schema, error) {
	rosterSchemaOnce.Do(func() {
		data, err := apischemas.FS.ReadFile(apischemas.FanOutRoster)
		if err != nil {
			rosterSchemaErr = fmt.Errorf("read embedded roster envelope: %w", err)
			return
		}
		rosterSchema, rosterSchemaErr = handoffcheck.Compile(apischemas.FanOutRoster, "", data)
	})
	return rosterSchema, rosterSchemaErr
}
