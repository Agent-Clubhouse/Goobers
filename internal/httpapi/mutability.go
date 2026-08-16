package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/readmodel"
)

// The mutability seam (#1934, design §7.4).
//
// # Mutation guarantees
//
// Alongside its read routes, the HTTP API serves three API-first run intervention
// routes: approve, override, and rerun. Each requires an idempotency key. This
// seam defines the requirements those mutations impose on the read model.
//
// Four things, each of which is a read-side constraint:
//
//   - read-your-write through source positions;
//   - required idempotency, because the interface's normal failure mode is a
//     client abort followed by the user clicking again;
//   - concurrency detection on definition edits;
//   - attribution.

// Headers carrying the seam.
const (
	// HeaderIfSourceApplied bounds a read on a source position:
	// `<runID>:<journalSeq>`.
	//
	// A SOURCE position, not a change sequence. The projector allocates
	// change.seq asynchronously AFTER a mutation returns, so a mutation cannot
	// hand one back — and making it wait would couple write availability to the
	// derived store, which is the dependency §3.1 exists to remove.
	HeaderIfSourceApplied = "If-Source-Applied"

	// HeaderIdempotencyKey is required on mutation routes.
	//
	// Required rather than optional, because the failure it prevents is the
	// interface's NORMAL failure mode: a client aborts (a 10s timeout, a closed
	// tab, a flaky link) and the user clicks the button again. An optional key
	// is absent exactly when the client is least reliable.
	HeaderIdempotencyKey = "Idempotency-Key"

	// HeaderSourceApplied reports the source position a mutation reached, so a
	// client can feed it straight back as If-Source-Applied.
	HeaderSourceApplied = "Source-Applied"

	// HeaderRetryAfterSeconds is sent with a lag refusal so a client knows to
	// wait rather than to give up.
	HeaderRetryAfterSeconds = "Retry-After"
)

// Wire codes for the seam's refusals. Stable strings: the client branches on
// them, and a renamed code silently stops being handled.
const (
	// CodeProjectionLagExceeded means the read model has not yet caught up to
	// the position the caller required.
	CodeProjectionLagExceeded = "projection_lag_exceeded"
	// CodeMalformedSourcePosition means the If-Source-Applied header could not
	// be parsed.
	CodeMalformedSourcePosition = "malformed_source_position"
	// CodeIdempotencyKeyRequired means a mutation arrived without one.
	CodeIdempotencyKeyRequired = "idempotency_key_required"
	// CodePreconditionRequired means a definition edit arrived without If-Match.
	CodePreconditionRequired = "precondition_required"
)

// idempotencyKeyMaxLen bounds what is accepted and durably recorded.
//
// A key is stored, so an unbounded one is an unbounded write from an
// unauthenticated header. 200 is generous for a UUID or a hash and small enough
// that a million of them is a rounding error.
const idempotencyKeyMaxLen = 200

// requireSourceApplied enforces an If-Source-Applied bound.
//
// Returns false when the response has already been written.
//
// # Why a lag refusal is 503 and not 409
//
// The caller asked for a bound the server cannot yet satisfy, and the correct
// client behaviour is to WAIT — the projector is running and will catch up.
// A 409 would read as "your request conflicts", inviting the client to change
// something, when nothing about the request is wrong.
func requireSourceApplied(
	w http.ResponseWriter,
	request *http.Request,
	reporter readmodel.FreshnessReporter,
) bool {
	raw := strings.TrimSpace(request.Header.Get(HeaderIfSourceApplied))
	if raw == "" {
		return true
	}
	required, err := readmodel.ParseSourceApplied(raw)
	if err != nil {
		// Malformed is a client error, and distinctly so: silently ignoring an
		// unparseable bound would tell the caller its read-your-write guarantee
		// held when nothing checked it.
		writeError(w, http.StatusBadRequest, CodeMalformedSourcePosition,
			"If-Source-Applied must be <runID>:<journalSeq>")
		return false
	}
	if reporter == nil {
		// No projection to compare against. Refusing is the honest answer: the
		// caller asked for a guarantee this topology cannot provide, and serving
		// the read anyway would be a silent no-op on an explicit request.
		writeError(w, http.StatusServiceUnavailable, CodeProjectionLagExceeded,
			"this deployment has no read model to satisfy If-Source-Applied")
		return false
	}

	satisfied, err := reporter.SatisfiesSourceApplied(request.Context(), required)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, CodeProjectionLagExceeded,
			"could not evaluate the required source position")
		return false
	}
	if !satisfied {
		w.Header().Set(HeaderRetryAfterSeconds, "1")
		writeError(w, http.StatusServiceUnavailable, CodeProjectionLagExceeded,
			fmt.Sprintf("the read model has not applied %s:%d yet",
				required.RunID, required.JournalSeq))
		return false
	}
	return true
}

// ErrIdempotencyKeyMissing reports a mutation with no key.
var ErrIdempotencyKeyMissing = errors.New("httpapi: Idempotency-Key is required on mutations")

// idempotencyKey extracts and validates the key from a mutation request.
func idempotencyKey(request *http.Request) (string, error) {
	key := strings.TrimSpace(request.Header.Get(HeaderIdempotencyKey))
	if key == "" {
		return "", ErrIdempotencyKeyMissing
	}
	if len(key) > idempotencyKeyMaxLen {
		return "", fmt.Errorf("httpapi: Idempotency-Key exceeds %d bytes", idempotencyKeyMaxLen)
	}
	// Control characters would corrupt a log line and could smuggle a newline
	// into a record. Rejected rather than sanitised: a caller sending one has a
	// bug, and quietly rewriting their key would break the idempotency they
	// asked for.
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("httpapi: Idempotency-Key contains control characters")
		}
	}
	return key, nil
}

// requireIdempotencyKey enforces the key on a mutation route.
//
// Returns the key, or false when the response has already been written.
func requireIdempotencyKey(w http.ResponseWriter, request *http.Request) (string, bool) {
	key, err := idempotencyKey(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeIdempotencyKeyRequired, err.Error())
		return "", false
	}
	return key, true
}

// requireIfMatch enforces a precondition on a definition edit.
//
// # Why required rather than optional
//
// An optional If-Match is absent precisely when two operators are editing at
// once — the case it exists to detect. Making it required means a concurrent
// edit is a 412 the second operator can act on, rather than a silent
// last-write-wins that neither of them observes.
func requireIfMatch(w http.ResponseWriter, request *http.Request, current string) bool {
	supplied := strings.TrimSpace(request.Header.Get("If-Match"))
	if supplied == "" {
		writeError(w, http.StatusBadRequest, CodePreconditionRequired,
			"If-Match is required; read the definition first and send its ETag")
		return false
	}
	if normaliseETag(supplied) != normaliseETag(current) {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed",
			"the definition changed since you read it; re-read and retry")
		return false
	}
	return true
}

// normaliseETag strips quoting and the weak marker so a client that echoes an
// ETag verbatim matches one that does not.
//
// Comparison is on the DIGEST, not on the formatting. A client library that
// re-quotes or drops the W/ prefix would otherwise fail a precondition that
// logically holds — a false conflict is as bad as a missed one, because the
// operator retries and hits it again.
func normaliseETag(tag string) string {
	tag = strings.TrimSpace(tag)
	tag = strings.TrimPrefix(tag, "W/")
	return strings.Trim(tag, `"`)
}

// definitionETag renders a digest as an ETag.
func definitionETag(digest string) string {
	if digest == "" {
		return ""
	}
	return `"` + digest + `"`
}
