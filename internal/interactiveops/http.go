package interactiveops

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// Handler uses relative routes under /api/v1/interactive/operations. Host must
// supply verified principal/session resolution and its own CSRF/origin policy.
func (s *Service) Handler(resolve ScopeResolver) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, err error) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		}
		if resolve == nil {
			fail(503, errors.New("session authorization unavailable"))
			return
		}
		scope, err := resolve(r.Context(), r.URL.Query().Get("sessionId"))
		if err != nil {
			fail(403, err)
			return
		}
		path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/interactive/operations"), "/")
		var result any
		switch {
		case r.Method == "GET" && (path == "state" || path == ""):
			result, err = s.Snapshot(r.Context(), scope)
		case r.Method == "POST" && path == "validate-workflow":
			var req struct {
				WorkflowYAML string `json:"workflowYaml"`
			}
			err = decode(r, &req)
			if err == nil {
				result, err = s.ValidateWorkflow(r.Context(), scope, req.WorkflowYAML)
			}
		case r.Method == "POST" && path == "proposals":
			var req Request
			err = decode(r, &req)
			if err == nil {
				result, err = s.Propose(r.Context(), scope, req)
			}
		case r.Method == "POST" && path == "chat":
			var req struct {
				Text           string `json:"text"`
				IdempotencyKey string `json:"idempotencyKey"`
			}
			err = decode(r, &req)
			if err == nil {
				result, err = s.Chat(r.Context(), scope, req.Text, req.IdempotencyKey)
			}
		case strings.HasPrefix(path, "actions/"):
			id := strings.TrimPrefix(path, "actions/")
			if r.Method == "POST" && strings.HasSuffix(id, "/apply") {
				result, err = s.Apply(r.Context(), scope, strings.TrimSuffix(id, "/apply"))
			} else if r.Method == "GET" && !strings.Contains(id, "/") {
				result, err = s.Get(r.Context(), scope, id)
			} else {
				fail(404, errors.New("unknown operation route"))
				return
			}
		default:
			fail(404, errors.New("unknown operation route"))
			return
		}
		if err != nil {
			fail(409, err)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	})
}
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 40000))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON request")
	}
	return nil
}

// Chat is an explicit local intent router, not a simulated LLM. It converts
// common helper requests into proposals that still need exact action Apply.
func (s *Service) Chat(ctx context.Context, scope Scope, text, key string) (Action, error) {
	state, err := s.Snapshot(ctx, scope)
	if err != nil {
		return Action{}, err
	}
	lower := strings.ToLower(strings.TrimSpace(text))
	req := Request{Gaggle: state.Policy.Gaggle, IdempotencyKey: key}
	switch {
	case strings.Contains(lower, "design workflow") || strings.Contains(lower, "add workflow") || strings.Contains(lower, "create workflow"):
		req.Kind = "add_workflow"
		req.WorkflowYAML = state.WorkflowDraft
	case strings.Contains(lower, "disable workflow") || strings.Contains(lower, "enable workflow"):
		req.Kind = "tweak_workflow"
		req.Workflow = workflowFromText(text, state)
		enabled := strings.Contains(lower, "enable workflow")
		req.Enabled = &enabled
	case strings.Contains(lower, "run workflow") || strings.Contains(lower, "submit run"):
		req.Kind = "submit_run"
		req.Workflow = workflowFromText(text, state)
	case strings.HasPrefix(lower, "rename gaggle "):
		req.Kind = "tweak_gaggle"
		name := strings.TrimSpace(text[len("rename gaggle "):])
		req.DisplayName = &name
	case strings.Contains(lower, "disable gaggle") || strings.Contains(lower, "enable gaggle"):
		req.Kind = "tweak_gaggle"
		enabled := strings.Contains(lower, "enable gaggle")
		req.Enabled = &enabled
	default:
		return Action{}, errors.New("supported local helper requests: design workflow, run workflow <name>, enable/disable workflow <name>, rename gaggle <display name>, enable/disable gaggle")
	}
	return s.Propose(ctx, scope, req)
}
func workflowFromText(text string, state State) string {
	defaultName := "invoice-check"
	if len(state.Workflows) > 0 {
		defaultName = state.Workflows[0].Name
		for _, wf := range state.Workflows {
			if wf.Name == "invoice-check" {
				defaultName = wf.Name
			}
		}
	}
	lower := strings.ToLower(text)
	at := strings.Index(lower, "workflow")
	if at < 0 {
		return defaultName
	}
	fields := strings.Fields(strings.TrimSpace(text[at+len("workflow"):]))
	if len(fields) == 0 {
		return defaultName
	}
	return fields[0]
}
