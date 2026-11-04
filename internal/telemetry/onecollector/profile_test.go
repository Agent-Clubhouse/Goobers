package onecollector

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProfileStrictAndLossless(t *testing.T) {
	raw := `{"profile_version":1,"enable_env":"SYNTHETIC_UPLOAD","endpoint":"https://collector.example.invalid/ingest","routing_key":{"env":"SYNTHETIC_ROUTE"},"instrumentation_key":{"file":"synthetic-key.txt"},"mappings":[{"signal":"journal","event_name":"Synthetic.Journal","envelope_version":"4.0","fields":{"body":{"source":"journal.raw_json"},"version":{"constant":"1"}}}]}`
	if _, err := ParseProfile([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		strings.Replace(raw, `"profile_version":1`, `"profile_version":1,"profile_version":1`, 1),
		strings.Replace(raw, `"profile_version":1`, `"profile_version":1,"unknown":true`, 1),
		strings.Replace(raw, `"env":"SYNTHETIC_ROUTE"`, `"value":"not-allowed"`, 1),
		strings.Replace(raw, "https://", "http://", 1),
		raw + "{}",
	} {
		if _, err := ParseProfile([]byte(invalid)); err == nil {
			t.Fatal("invalid profile accepted")
		}
	}
	object, err := DecodeObject(`{"exact":9223372036854775807}`, 1024)
	if err != nil || object["exact"].(json.Number).String() != "9223372036854775807" {
		t.Fatal(object, err)
	}
}

func TestProjectionAliasesAndTypes(t *testing.T) {
	mapping := Mapping{EventName: "Synthetic.Point", EnvelopeVersion: "4.0", Fields: map[string]Field{"value": {Source: "value"}},
		Projections: []Projection{{Source: "attributes", Prefix: "attr_", KeyEncoding: "hex", TypePrefix: "type_", Aliases: map[string]string{"label": "label", "count": "count", "list": "list"}}}}
	facts := Facts{"value": JSON(int64(9223372036854775807)), "attributes.label": JSON("hello"), "attributes.count": JSON(7),
		"attributes.list": JSON(`{"arrayValue":{"values":[]}}`), "attributes.list.otlp_type": JSON("arrayValue")}
	envelope, err := mapping.Envelope(facts, "synthetic", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(map[string]any)
	if _, ok := data["label"]; !ok {
		t.Fatal("string alias absent")
	}
	if _, ok := data["count"]; ok {
		t.Fatal("numeric attribute used string alias")
	}
	if _, ok := data["type_attr_6c697374"]; !ok {
		t.Fatal("complex companion key absent")
	}
	raw, err := Serialize([]Envelope{envelope})
	if err != nil || !strings.Contains(string(raw), "9223372036854775807") {
		t.Fatal(string(raw), err)
	}
}

func TestTransportOptInStatusAndPrivacy(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("apikey") != "synthetic-route" {
			t.Error("routing not supplied")
		}
		w.Header().Set("Location", "https://example.invalid/leak")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	options := Options{EnableEnv: "SYNTHETIC_UPLOAD", Endpoint: server.URL, RoutingKey: "synthetic-route", InstrumentationKey: "synthetic-key"}
	record := []Envelope{{Name: "Synthetic.Event", Data: map[string]any{"value": 1}}}
	for _, value := range []string{"", "true", "0"} {
		t.Setenv(options.EnableEnv, value)
		if _, err := SendResult(t.Context(), options, record, server.Client().Transport); !errors.Is(err, ErrDisabled) {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal("disabled transport sent data")
	}
	t.Setenv(options.EnableEnv, "1")
	_, err := SendResult(t.Context(), options, record, server.Client().Transport)
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") || strings.Contains(err.Error(), "synthetic-route") {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("requests=%d, expected no retry/redirect", calls)
	}
}

func TestLifecycleCompletedWins(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 100 {
		result := make(chan error, 1)
		result <- nil
		if err := AwaitLifecycle(ctx, result); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHistogramReplayBounds(t *testing.T) {
	sum, min, max := 6.0, 1.0, 3.0
	segments, err := Replay(3, []uint64{1, 2}, []float64{1}, &sum, &min, &max, true)
	if err != nil {
		t.Fatal(err)
	}
	var count uint64
	total := 0.0
	for _, segment := range segments {
		count += segment.Count
		total += segment.Value * float64(segment.Count)
	}
	if count != 3 || math.Abs(total-sum) > 1e-14 {
		t.Fatal(count, total)
	}
	if _, err := Replay(4, []uint64{1, 2}, []float64{1}, &sum, &min, &max, true); err == nil {
		t.Fatal("count mismatch accepted")
	}
}
