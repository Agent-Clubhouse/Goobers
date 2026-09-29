package decider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, APIKey: "test-key", Model: "m", Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func questions() map[string]Question {
	return map[string]Question{
		"ok":    Noul("Is it fine?", &NoulCriteria{True: "fine", False: "not fine"}),
		"route": Choice("Where next?", map[string]any{"a": "first", "b": nil}),
		"qual":  Score("How good?", []any{"bad", "ok", "good"}),
	}
}

const goodBody = `{"model":"jev-x","answers":{
 "ok":{"type":"noul","noul":0.9},
 "route":{"type":"choice","choice":"a","probabilities":{"a":0.8,"b":0.2},"confidence":0.7},
 "qual":{"type":"score","score":1.4,"legend":{"0":"bad","1":"ok","2":"good"},"probabilities":{"0":0,"1":0.6,"2":0.4},"confidence":0.8}},
 "usage":{"input_tokens":10,"output_tokens":3}}`

func TestDecideRoundTrip(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("bad auth header")
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(goodBody))
	})
	resp, err := c.Decide(context.Background(), Request{State: map[string]any{"x": 1}, Questions: questions()})
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "m" {
		t.Errorf("model not sent: %v", got["model"])
	}
	qs := got["questions"].(map[string]any)
	if qs["route"].(map[string]any)["type"] != "choice" || qs["qual"].(map[string]any)["criteria"] == nil {
		t.Errorf("wire questions wrong: %v", qs)
	}
	if *resp.Answers["ok"].Yes != 0.9 || resp.Answers["route"].Choice != "a" || *resp.Answers["qual"].Score != 1.4 || resp.Usage.InputTokens != 10 {
		t.Errorf("decoded wrong: %+v", resp)
	}
}

func TestRetriesTransientThenSucceeds(t *testing.T) {
	var n atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(529)
			return
		}
		_, _ = w.Write([]byte(goodBody))
	})
	if _, err := c.Decide(context.Background(), Request{State: "s", Questions: questions()}); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts = %d", n.Load())
	}
}

func TestDoesNotRetryClientErrorsAndHidesKey(t *testing.T) {
	var n atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	})
	_, err := c.Decide(context.Background(), Request{State: "s", Questions: questions()})
	se, ok := err.(*StatusError)
	if !ok || se.StatusCode != 401 || n.Load() != 1 {
		t.Fatalf("err=%v attempts=%d", err, n.Load())
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Fatal("key leaked in error")
	}
}

func TestRejectsMalformedAnswers(t *testing.T) {
	cases := map[string]string{
		"missing answer":    `{"answers":{"ok":{"type":"noul","noul":0.5}}}`,
		"wrong type":        strings.Replace(goodBody, `"ok":{"type":"noul","noul":0.9}`, `"ok":{"type":"score","score":1}`, 1),
		"noul out of range": strings.Replace(goodBody, `"noul":0.9`, `"noul":1.7`, 1),
		"unoffered choice":  strings.Replace(goodBody, `"choice":"a"`, `"choice":"zzz"`, 1),
		"bad distribution":  strings.Replace(goodBody, `"a":0.8,"b":0.2`, `"a":0.1,"b":0.2`, 1),
		"score off scale":   strings.Replace(goodBody, `"score":1.4`, `"score":7`, 1),
		"no confidence":     strings.Replace(goodBody, `"confidence":0.7`, `"confidence":null`, 1),
		"not json":          `<html>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
			if _, err := c.Decide(context.Background(), Request{State: "s", Questions: questions()}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestValidationAndConfig(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("must not call server") })
	for name, q := range map[string]Question{
		"one option":  Choice("x", map[string]any{"a": nil}),
		"one level":   Score("x", []any{"a"}),
		"no instruct": Noul("", nil),
	} {
		if _, err := c.Decide(context.Background(), Request{State: "s", Questions: map[string]Question{"q": q}}); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := c.Decide(context.Background(), Request{Questions: questions()}); err == nil {
		t.Error("nil state must fail")
	}
	for _, cfg := range []Config{
		{BaseURL: "http://example.com", APIKey: "k", Model: "m"},
		{BaseURL: "https://x", APIKey: "", Model: "m"},
		{BaseURL: "not a url", APIKey: "k", Model: "m"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("config %+v should fail", cfg.BaseURL)
		}
	}
}

func TestContextCancelStopsRetries(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(529) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Decide(ctx, Request{State: "s", Questions: questions()}); err == nil {
		t.Fatal("expected error")
	}
}

// TestLiveEndpoint is a manual smoke test. It runs only when the environment
// provides an endpoint; nothing here embeds deployment values.
func TestLiveEndpoint(t *testing.T) {
	base, key, model := os.Getenv("TYPESAFE_BASE_URL"), os.Getenv("TYPESAFE_API_KEY"), os.Getenv("TYPESAFE_DEFAULT_MODEL")
	if base == "" || key == "" || model == "" {
		t.Skip("TYPESAFE_BASE_URL, TYPESAFE_API_KEY, TYPESAFE_DEFAULT_MODEL not set")
	}
	c, err := New(Config{BaseURL: base, APIKey: key, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cases := []struct {
		name, state string
		wantYes     bool
	}{
		{"complete", `{"verdict":"pass","summary":"All 12 checks passed.","items":[{"id":1},{"id":2}]}`, false},
		{"refusal", `I can't process this input because the JSON appears corrupted and incomplete.`, true},
	}
	for _, tc := range cases {
		resp, err := c.Decide(ctx, Request{State: tc.state, Questions: map[string]Question{
			"claims_bad_input": Noul("Does the text claim its input is corrupted, incomplete, or unreadable?", nil),
			"kind": Choice("What is this text?", map[string]any{
				"structured_result": "A structured result with fields and values",
				"refusal_or_error":  "A message declining or reporting a problem instead of a result",
			}),
		}})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		a := resp.Answers
		t.Logf("%s: claims_bad_input=%.3f kind=%s (conf %.2f) model=%s tokens=%d/%d", tc.name, *a["claims_bad_input"].Yes, a["kind"].Choice, *a["kind"].Confidence, resp.Model, resp.Usage.InputTokens, resp.Usage.OutputTokens)
		if got := *a["claims_bad_input"].Yes > 0.5; got != tc.wantYes {
			t.Errorf("%s: claims_bad_input yes=%v, want %v", tc.name, got, tc.wantYes)
		}
	}
}
