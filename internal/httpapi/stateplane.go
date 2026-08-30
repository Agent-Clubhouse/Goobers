package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/stateclient"
)

// stateplane.go implements the daemon write API's scheduler-state plane
// (decision 005 ruling 3, finding 002 "plane clients" §3 / plan step C2): ONE
// small gaggle-scoped key/value route for the scheduler state that is NOT a
// claim — blocked.json's learned-dependency records, the per-scan backlog
// cursor (#2067), the reconcile-post-merge ledger, and the
// gather-sibling-context cache.
//
// The daemon is not a second store and not a second coordinator. It serves the
// SAME files under the SAME per-key locks the in-process path takes: for
// blocked.json and the scan cursors that is claims.lock (blockedrecords.go's
// updateBlockedRecords and backlogquery.go's advanceBacklogScanCursor both
// hold it), which is precisely what keeps a runner-driven 2.0 run in the
// daemon's own process and an engine-driven 3.0 run in a pod inside ONE
// atomicity domain rather than two. Finding 002's "Lane flip under WF-016"
// hazard is correct only if that is tested, so it is
// (cmd/goobers/schedulerstate_test.go's same-lock interleaving cases).
//
// CAS, NOT LAST-WRITE-WINS. In-process a read-modify-write is one lock
// acquisition. Over the plane the read and the write are separate round trips,
// so the write carries the read's ETag:
//   - GET answers 200 with the value's bytes and its ETag, or 404 when the key
//     is absent (the first-run state of every one of these files).
//   - PUT requires a precondition — `If-Match: "<etag>"` to replace exactly
//     the version that was read, or `If-None-Match: *` to create a key that
//     must still be absent. An UNCONDITIONAL PUT IS REFUSED: a blind overwrite
//     is the lost update this whole route exists to make impossible.
//   - A precondition that no longer holds is 412, which the client's Update
//     retries against the new value.
//
// CONTAINMENT IS FAIL-CLOSED, twice over. The key namespace is closed
// (stateclient.ValidKey): the four state shapes and nothing else, so a
// state-plane bearer can never be turned into a read or a write of
// claims.json, the instance config, or any other file that happens to live in
// the scheduler directory. And a pod principal may address ONLY the gaggle its
// own run belongs to — the service verifies membership, and an unverifiable
// gaggle is refused rather than resolved.

// MaxStateValueBytes bounds one scheduler-state value, restating the client's
// own cap (stateclient.MaxValueBytes); pinned against it by this package's
// tests.
const MaxStateValueBytes = stateclient.MaxValueBytes

// StateGetRequest reads one scheduler-state key.
type StateGetRequest struct {
	// Gaggle is the route's scope, taken from the path.
	Gaggle string
	// Key is the scheduler-state key, taken from the path and already checked
	// against the closed namespace by the route.
	Key string
	// PodScoped is set by the route, never decoded from a body: the caller is
	// a pod principal, so the service must additionally verify that RunID's
	// run belongs to Gaggle.
	PodScoped bool
	// RunID is the calling pod's own run, taken from the principal.
	RunID string
}

// StatePutRequest writes one scheduler-state key under a precondition.
type StatePutRequest struct {
	StateGetRequest
	// Data is the value's new bytes.
	Data []byte
	// IfMatch is the ETag the caller read. Empty means the caller sent
	// `If-None-Match: *` — the key must still be absent.
	IfMatch string
}

// StateValue is one scheduler-state read or write acknowledgement.
type StateValue struct {
	Data []byte
	// ETag is the value's content digest. Empty only for an absent key, which
	// Get reports through Found=false rather than through an empty ETag.
	ETag  string
	Found bool
}

// ErrStatePrecondition is the service's refusal for a compare-and-swap whose
// precondition no longer holds — mapped to 412 by the route. Distinct from
// an InterventionError so a service cannot accidentally spell a lost update
// as a 500.
var ErrStatePrecondition = errors.New("scheduler-state precondition failed")

// StateService is the daemon-side scheduler-state plane. Implementations serve
// each key from the instance's own scheduler directory under that key's
// existing cross-process lock — the plane is transport in front of the same
// files the daemon's scheduler and the local CLI seams use, never a second
// copy.
type StateService interface {
	GetState(ctx context.Context, request StateGetRequest) (StateValue, error)
	PutState(ctx context.Context, request StatePutRequest) (StateValue, error)
}

// WithStateService enables the scheduler-state plane's routes.
func WithStateService(state StateService) HandlerOption {
	return func(config *handlerConfig) error {
		if state == nil {
			return errors.New("http API scheduler-state service is required")
		}
		config.state = state
		return nil
	}
}

// registerStatePlaneRoutes registers GET and PUT on the shared gaggle-state
// path (HandleByMethod, since net/http's ServeMux rejects two bare
// registrations of the same pattern). Registered unconditionally, like the
// claims and blob planes: a nil service answers a structured 503 rather than
// the routes silently not existing.
func registerStatePlaneRoutes(router *Router, state StateService, errorLog *log.Logger) {
	router.HandleByMethod(
		map[string]apicontract.RouteID{
			http.MethodGet: apicontract.RouteGaggleStateGet,
			http.MethodPut: apicontract.RouteGaggleStatePut,
		},
		map[apicontract.RouteID]http.HandlerFunc{
			apicontract.RouteGaggleStateGet: stateGetHandler(state, errorLog),
			apicontract.RouteGaggleStatePut: statePutHandler(state, errorLog),
		},
	)
}

// stateRequestScope validates the path pair and binds the caller's identity.
// It is where the plane's containment starts: a key outside the closed
// namespace is refused before anything resolves it, and a pod principal is
// flagged PodScoped with its own run id so the service can verify the run
// actually belongs to the gaggle in the path.
func stateRequestScope(w http.ResponseWriter, request *http.Request) (StateGetRequest, bool) {
	gaggle := request.PathValue("gaggle")
	key := request.PathValue("key")
	if strings.TrimSpace(gaggle) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "gaggle is required")
		return StateGetRequest{}, false
	}
	if !stateclient.ValidKey(key) {
		// Fail closed on the key BEFORE any path is built from it: the closed
		// namespace is what stops a state bearer becoming a read of
		// claims.json or the instance config.
		writeError(w, http.StatusBadRequest, "invalid_state_key",
			"key is not a scheduler-state key")
		return StateGetRequest{}, false
	}
	scope := StateGetRequest{Gaggle: gaggle, Key: key}
	if principal, ok := PrincipalFromRequest(request); ok && IsPodPrincipal(principal) {
		runID, ok := podPrincipalRunID(principal)
		if !ok {
			writeError(w, http.StatusForbidden, "run_mismatch",
				"pod principal does not name a run")
			return StateGetRequest{}, false
		}
		scope.PodScoped = true
		scope.RunID = runID
	}
	return scope, true
}

// podPrincipalRunID recovers the run a pod principal was minted for from its
// subject (the inverse of podPrincipalSubject).
func podPrincipalRunID(principal Principal) (string, bool) {
	runID, ok := strings.CutPrefix(principal.Subject, "run:")
	if !ok || runID == "" {
		return "", false
	}
	return runID, true
}

func stateGetHandler(state StateService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if state == nil {
			writeError(w, http.StatusServiceUnavailable, "state_unavailable", "the scheduler-state plane is not available from this server")
			return
		}
		scope, ok := stateRequestScope(w, request)
		if !ok {
			return
		}
		value, err := state.GetState(request.Context(), scope)
		if err != nil {
			writeStatePlaneError(w, errorLog, "read scheduler state", err)
			return
		}
		if !value.Found {
			writeError(w, http.StatusNotFound, "not_found", "scheduler-state key not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(value.Data)))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Scheduler state is mutable by definition: an intermediary that
		// cached it would hand a CAS loop a stale ETag and spin it forever.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("ETag", `"`+value.ETag+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(value.Data)
	}
}

func statePutHandler(state StateService, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if state == nil {
			writeError(w, http.StatusServiceUnavailable, "state_unavailable", "the scheduler-state plane is not available from this server")
			return
		}
		scope, ok := stateRequestScope(w, request)
		if !ok {
			return
		}
		ifMatch, ok := statePrecondition(w, request)
		if !ok {
			return
		}
		defer func() { _ = request.Body.Close() }()
		data, err := io.ReadAll(http.MaxBytesReader(w, request.Body, MaxStateValueBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "state_value_too_large",
					fmt.Sprintf("scheduler-state value exceeds %d bytes", MaxStateValueBytes))
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_request", "scheduler-state body could not be read")
			return
		}
		value, err := state.PutState(request.Context(), StatePutRequest{
			StateGetRequest: scope, Data: data, IfMatch: ifMatch,
		})
		if err != nil {
			writeStatePlaneError(w, errorLog, "write scheduler state", err)
			return
		}
		w.Header().Set("ETag", `"`+value.ETag+`"`)
		w.WriteHeader(http.StatusNoContent)
	}
}

// statePrecondition reads the write's compare-and-swap precondition. EXACTLY
// ONE of If-Match (replace this version) and If-None-Match: * (create, must
// still be absent) is required: an unconditional PUT is the blind overwrite
// this route exists to make impossible, and both at once is a caller that does
// not know which it meant.
func statePrecondition(w http.ResponseWriter, request *http.Request) (string, bool) {
	ifMatch := strings.TrimSpace(request.Header.Get(stateclient.HeaderIfMatch))
	ifNoneMatch := strings.TrimSpace(request.Header.Get(stateclient.HeaderIfNoneMatch))
	switch {
	case ifMatch == "" && ifNoneMatch == "":
		writeError(w, http.StatusPreconditionRequired, "precondition_required",
			"scheduler-state writes require If-Match: \"<etag>\" or If-None-Match: *")
		return "", false
	case ifMatch != "" && ifNoneMatch != "":
		writeError(w, http.StatusBadRequest, "invalid_request",
			"send either If-Match or If-None-Match, not both")
		return "", false
	case ifNoneMatch != "":
		if ifNoneMatch != stateclient.IfNoneMatchAny {
			writeError(w, http.StatusBadRequest, "invalid_request",
				"If-None-Match must be *")
			return "", false
		}
		return "", true
	}
	// A wildcard If-Match ("replace whatever is there") is precisely the
	// unconditional write the plane refuses; a multi-tag If-Match has no
	// meaning for a single-version key. Both fail closed.
	etag := strings.Trim(ifMatch, `"`)
	if ifMatch == stateclient.IfNoneMatchAny || strings.ContainsAny(etag, `",`) || etag == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"If-Match must be a single quoted entity tag")
		return "", false
	}
	return etag, true
}

// writeStatePlaneError maps a scheduler-state failure onto the shared error
// envelope. A lost compare-and-swap is 412 (the caller re-reads and retries),
// a typed refusal passes through, and everything else is a 500 that never
// leaks internals.
func writeStatePlaneError(w http.ResponseWriter, errorLog *log.Logger, operation string, err error) {
	if errors.Is(err, ErrStatePrecondition) {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed",
			"the scheduler-state value changed since it was read")
		return
	}
	writePlaneError(w, errorLog, operation, err)
}
