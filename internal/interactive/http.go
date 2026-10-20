package interactive

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiPrefix = "/api/v1/interactive"

// Handler serves the interactive HTTP and event-stream endpoints.
func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }
func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	owner, err := s.opts.Principal(r)
	if err != nil || owner == "" {
		writeError(w, problem(401, "unauthenticated", "authenticated owner is required"))
		return
	}
	if r.Method != "GET" {
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, e := url.Parse(origin)
			if e != nil || parsed.Host != r.Host {
				writeError(w, problem(403, "origin_denied", "cross-origin session changes are denied"))
				return
			}
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeError(w, problem(415, "content_type", "application/json is required"))
			return
		}
	}
	path := strings.TrimPrefix(r.URL.Path, apiPrefix)
	if path == r.URL.Path {
		writeError(w, problem(404, "not_found", "unknown interactive route"))
		return
	}
	switch path {
	case "/surfaces":
		if r.Method != "GET" {
			methodError(w)
			return
		}
		s.mu.Lock()
		surfaces := []Surface{}
		for _, surface := range s.opts.Surfaces {
			if surface.Enabled {
				surfaces = append(surfaces, clone(surface))
			}
		}
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"surfaces": surfaces, "executionModes": s.ExecutionModes(), "defaultExecutionMode": s.opts.DefaultExecutionMode})
		return
	case "/sessions":
		switch r.Method {
		case "GET":
			writeJSON(w, 200, map[string]any{"sessions": s.ListSessions(owner)})
		case "POST":
			var req CreateRequest
			if err := decode(r, &req); err != nil {
				writeError(w, err)
				return
			}
			session, err := s.Create(owner, req)
			if err != nil {
				writeError(w, err)
				return
			}
			writeJSON(w, 201, session)
		default:
			methodError(w)
		}
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 2 || parts[0] != "sessions" || parts[1] == "" {
		writeError(w, problem(404, "not_found", "unknown interactive route"))
		return
	}
	sessionID := parts[1]
	if len(parts) == 2 && r.Method == "GET" {
		session, err := s.Session(owner, sessionID)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, session)
		return
	}
	if len(parts) != 3 {
		writeError(w, problem(404, "not_found", "unknown interactive route"))
		return
	}
	switch parts[2] {
	case "commands":
		if r.Method != "POST" {
			methodError(w)
			return
		}
		var cmd Command
		if err := decode(r, &cmd); err != nil {
			writeError(w, err)
			return
		}
		receipt, err := s.Command(r.Context(), owner, sessionID, cmd)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 202, receipt)
	case "changes":
		if r.Method != "GET" {
			methodError(w)
			return
		}
		session, err := s.Session(owner, sessionID)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"changes": session.Changes, "diff": session.Diff, "workspacePath": session.WorkspacePath})
	case "events":
		if r.Method != "GET" {
			methodError(w)
			return
		}
		s.eventsHTTP(w, r, owner, sessionID)
	default:
		writeError(w, problem(404, "not_found", "unknown interactive route"))
	}
}
func decode(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 128<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return problem(400, "invalid_json", "request body must be a valid bounded JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return problem(400, "invalid_json", "request body must contain exactly one object")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, err error) {
	var api *APIError
	if !errors.As(err, &api) {
		api = &APIError{503, "unavailable", "interactive service unavailable"}
	}
	writeJSON(w, api.Status, map[string]any{"error": api})
}
func methodError(w http.ResponseWriter) {
	writeError(w, problem(405, "method_not_allowed", "method is not supported"))
}

// Events returns the owner-visible durable events after a sequence cursor.
func (s *Service) Events(owner, sessionID string, after uint64) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.record(owner, sessionID)
	if err != nil {
		return nil, err
	}
	if after > rec.Session.Sequence {
		return nil, problem(409, "future_cursor", "event cursor is ahead of this session")
	}
	events := []Event{}
	for _, event := range rec.Events {
		if event.Sequence > after {
			events = append(events, clone(event))
		}
	}
	return events, nil
}
func (s *Service) eventsHTTP(w http.ResponseWriter, r *http.Request, owner, sessionID string) {
	cursor := r.URL.Query().Get("after")
	if cursor == "" {
		cursor = r.Header.Get("Last-Event-ID")
	}
	var after uint64
	var err error
	if cursor != "" {
		after, err = strconv.ParseUint(cursor, 10, 64)
	}
	if err != nil {
		writeError(w, problem(400, "invalid_cursor", "event cursor must be an unsigned sequence"))
		return
	}
	events, err := s.Events(owner, sessionID, after)
	if err != nil {
		writeError(w, err)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		writeJSON(w, 200, map[string]any{"events": events})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, problem(503, "stream_unavailable", "response streaming is unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	heartbeat := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()
	for {
		// Capture wakeup before reading, so an event cannot fall between replay and
		// subscription. Re-authorize before every batch and every heartbeat.
		s.mu.Lock()
		changed := s.changed
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		current, authErr := s.opts.Principal(r)
		if authErr != nil || current != owner {
			return
		}
		events, err = s.Events(owner, sessionID, after)
		if err != nil {
			return
		}
		for _, event := range events {
			b, _ := json.Marshal(event)
			if _, err := fmt.Fprintf(w, "id: %d\nevent: interactive\ndata: %s\n\n", event.Sequence, b); err != nil {
				return
			}
			after = event.Sequence
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
