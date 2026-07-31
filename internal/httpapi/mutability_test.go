package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/readmodel"
)

// stubReporter satisfies readmodel.FreshnessReporter with a fixed applied
// position.
type stubReporter struct {
	applied uint64
	err     error
}

func (s stubReporter) ReadState(context.Context, readmodel.ReadStateInput) (readmodel.ReadState, error) {
	return readmodel.ReadState{}, s.err
}

func (s stubReporter) SourceApplied(_ context.Context, runID string) (readmodel.SourcePosition, bool, error) {
	if s.err != nil {
		return readmodel.SourcePosition{}, false, s.err
	}
	return readmodel.SourcePosition{RunID: runID, JournalSeq: s.applied}, true, nil
}

func (s stubReporter) SatisfiesSourceApplied(_ context.Context, required readmodel.SourcePosition) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.applied >= required.JournalSeq, nil
}

func errorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope ErrorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return envelope.Error.Code
}

// TestSourceAppliedRefusalIs503NotConflict pins the status choice.
//
// The caller asked for a bound the server cannot YET satisfy, and the correct
// client behaviour is to wait — the projector is running and will catch up. A
// 409 would read as "your request conflicts", inviting the client to change
// something, when nothing about the request is wrong.
func TestSourceAppliedRefusalIs503NotConflict(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	request.Header.Set(HeaderIfSourceApplied, "run-a:99")

	if requireSourceApplied(response, request, stubReporter{applied: 40}) {
		t.Fatal("a lagging projection satisfied a bound it has not reached")
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; a 409 would tell the client to change its request "+
			"when nothing about it is wrong", response.Code)
	}
	if code := errorCode(t, response); code != CodeProjectionLagExceeded {
		t.Errorf("code = %q, want %q", code, CodeProjectionLagExceeded)
	}
	if response.Header().Get(HeaderRetryAfterSeconds) == "" {
		t.Error("no Retry-After; the client is told to wait but not how long")
	}
}

// TestSourceAppliedPassesWhenCaughtUp pins the satisfied case, including the
// boundary.
func TestSourceAppliedPassesWhenCaughtUp(t *testing.T) {
	for _, required := range []string{"run-a:40", "run-a:39"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
		request.Header.Set(HeaderIfSourceApplied, required)
		if !requireSourceApplied(response, request, stubReporter{applied: 40}) {
			t.Errorf("%s was refused against an applied position of 40", required)
		}
	}
}

// TestMalformedSourcePositionIsRejectedNotIgnored is the important negative.
//
// Silently ignoring an unparseable bound would tell the caller its
// read-your-write guarantee held when nothing checked it — the guarantee
// inverted, which is worse than not offering one.
func TestMalformedSourcePositionIsRejectedNotIgnored(t *testing.T) {
	for _, raw := range []string{"run-a", "run-a:", ":40", "run-a:xyz", "run-a:40:extra"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
		request.Header.Set(HeaderIfSourceApplied, raw)

		if requireSourceApplied(response, request, stubReporter{applied: 999}) {
			t.Errorf("%q was accepted; a bound nobody could parse must never read as satisfied", raw)
			continue
		}
		if response.Code != http.StatusBadRequest {
			t.Errorf("%q gave status %d, want 400", raw, response.Code)
		}
		if code := errorCode(t, response); code != CodeMalformedSourcePosition {
			t.Errorf("%q gave code %q, want %q", raw, code, CodeMalformedSourcePosition)
		}
	}
}

// TestAbsentHeaderIsAllowed pins that the bound is opt-in.
//
// Every ordinary page view sends no header and must not be refused; strictness
// is per-request, which is what lets serve-labelled-stale be the default.
func TestAbsentHeaderIsAllowed(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	if !requireSourceApplied(response, request, stubReporter{applied: 0}) {
		t.Error("a request with no If-Source-Applied was refused")
	}
}

// TestNoReporterRefusesRatherThanSilentlyPassing pins the topology case.
//
// A deployment with no read model cannot satisfy the bound. Serving the read
// anyway would be a silent no-op on an explicit request — the caller believes
// its write is visible when nothing verified it.
func TestNoReporterRefusesRatherThanSilentlyPassing(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	request.Header.Set(HeaderIfSourceApplied, "run-a:1")

	if requireSourceApplied(response, request, nil) {
		t.Error("a deployment with no read model satisfied an If-Source-Applied bound")
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", response.Code)
	}
}

// TestIdempotencyKeyIsValidatedNotJustPresent pins the bounds on a header that
// gets durably recorded.
//
// An unbounded key is an unbounded write from an unauthenticated header; a key
// with control characters could corrupt a log line or smuggle a newline into a
// record.
func TestIdempotencyKeyIsValidatedNotJustPresent(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/x", nil)
		request.Header.Set(HeaderIdempotencyKey, "01J8ZQ7X9K")
		key, err := idempotencyKey(request)
		if err != nil || key != "01J8ZQ7X9K" {
			t.Errorf("key = %q err = %v", key, err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/x", nil)
		if _, err := idempotencyKey(request); err == nil {
			t.Error("a mutation with no key was accepted; the retry-after-abort case is the " +
				"interface's normal failure mode")
		}
	})
	t.Run("oversized", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/x", nil)
		request.Header.Set(HeaderIdempotencyKey, strings.Repeat("k", idempotencyKeyMaxLen+1))
		if _, err := idempotencyKey(request); err == nil {
			t.Error("an oversized key was accepted; it is durably recorded, so this is an " +
				"unbounded write from an unauthenticated header")
		}
	})
	t.Run("control characters", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/x", nil)
		request.Header.Set(HeaderIdempotencyKey, "abc\x01def")
		if _, err := idempotencyKey(request); err == nil {
			t.Error("a key with control characters was accepted")
		}
	})
}

// TestIfMatchNormalisesQuotingAndWeakMarkers pins that a false conflict is
// treated as seriously as a missed one.
//
// A client library that re-quotes an ETag, or drops the W/ prefix, would
// otherwise fail a precondition that logically holds — and the operator retries
// and hits it again.
func TestIfMatchNormalisesQuotingAndWeakMarkers(t *testing.T) {
	const digest = "sha256:abc123"
	for _, supplied := range []string{digest, `"` + digest + `"`, `W/"` + digest + `"`} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/x", nil)
		request.Header.Set("If-Match", supplied)
		if !requireIfMatch(response, request, definitionETag(digest)) {
			t.Errorf("If-Match %q was rejected against the same digest; a false conflict "+
				"loops the operator", supplied)
		}
	}
}

// TestIfMatchIsRequiredNotOptional pins the decision that makes concurrent edits
// detectable.
//
// An optional precondition is absent precisely when two operators are editing at
// once — the case it exists to detect. Required means the second gets a 4xx they
// can act on rather than a silent last-write-wins that neither observes.
func TestIfMatchIsRequiredNotOptional(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/x", nil)
	if requireIfMatch(response, request, definitionETag("sha256:abc")) {
		t.Fatal("an edit with no If-Match was allowed; two concurrent operators would " +
			"silently overwrite each other")
	}
	if code := errorCode(t, response); code != CodePreconditionRequired {
		t.Errorf("code = %q, want %q", code, CodePreconditionRequired)
	}
}

// TestIfMatchMismatchIs412 pins the conflict status.
func TestIfMatchMismatchIs412(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/x", nil)
	request.Header.Set("If-Match", definitionETag("sha256:stale"))
	if requireIfMatch(response, request, definitionETag("sha256:current")) {
		t.Fatal("a stale If-Match was accepted")
	}
	if response.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412", response.Code)
	}
}

// TestRequireIdempotencyKeyWritesATypedRefusal covers the HTTP-level wrapper.
//
// Separate from the validation test above because the failure mode differs: a
// caller needs a specific code to distinguish "you forgot the header" from a
// generic 400, or its retry logic cannot tell a fixable client bug from a
// server problem.
func TestRequireIdempotencyKeyWritesATypedRefusal(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/x", nil)
		if _, ok := requireIdempotencyKey(response, request); ok {
			t.Fatal("a mutation with no Idempotency-Key was allowed through")
		}
		if response.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", response.Code)
		}
		if code := errorCode(t, response); code != CodeIdempotencyKeyRequired {
			t.Errorf("code = %q, want %q; without a specific code a client cannot tell a "+
				"fixable client bug from a server problem", code, CodeIdempotencyKeyRequired)
		}
	})
	t.Run("present", func(t *testing.T) {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/x", nil)
		request.Header.Set(HeaderIdempotencyKey, "key-1")
		key, ok := requireIdempotencyKey(response, request)
		if !ok || key != "key-1" {
			t.Errorf("key = %q ok = %v, want key-1/true", key, ok)
		}
	})
}
