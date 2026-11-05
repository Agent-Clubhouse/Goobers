package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestOneCollectorExplicitCLIAndMCPValidationOnly(t *testing.T) {
	path := writeSyntheticCollectorProfile(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var profile map[string]any
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	profile["inputs"] = []any{map[string]any{"name": "synthetic", "schema": map[string]any{"type": "object", "required": []string{"value"}, "additionalProperties": false, "properties": map[string]any{"value": map[string]any{"type": "integer"}}}}}
	profile["tools"] = []any{map[string]any{"name": "synthetic_upload", "description": "Synthetic test tool", "input": "synthetic", "payload_path": "event", "validation_field": "check", "input_schema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"event"}, "properties": map[string]any{"event": map[string]any{"type": "object"}, "check": map[string]any{"type": "boolean"}}}}}
	profile["mappings"] = []any{map[string]any{"signal": "input", "event_name": "Synthetic.Input", "envelope_version": "4.0", "fields": map[string]any{"value": map[string]string{"source": "input.value"}}}}
	raw, err = json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNTHETIC_UPLOAD", "")
	t.Setenv("SYNTHETIC_ROUTE", "")
	t.Setenv("SYNTHETIC_KEY", "")
	var stdout, stderr bytes.Buffer
	if code := runOneCollectorUploadWithInput([]string{"--profile-file", path, "--input", "synthetic", "--validate-only"}, strings.NewReader(`{"value":9223372036854775807}`), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), `"category":"validated"`) {
		t.Fatal(code, stdout.String(), stderr.String())
	}
	stdin, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(`{"value":1}`); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	originalStdin := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = originalStdin; _ = stdin.Close() })
	stdout.Reset()
	if code := run([]string{"telemetry", "onecollector", "upload", "--profile-file", path, "--input", "synthetic", "--validate-only"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), `"category":"validated"`) {
		t.Fatal(code, stdout.String(), stderr.String())
	}
	messages := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"ping"}
{"jsonrpc":"2.0","id":3,"method":"tools/list"}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"synthetic_upload","arguments":{"check":true,"event":{"value":9223372036854775807}}}}
{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"synthetic_upload","arguments":{"check":"true","event":{}}}}
{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"unknown","arguments":{}}}
{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"synthetic_upload","arguments":{},"unknown":true}}
{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"synthetic_upload","arguments":{"event":{"value":1}}}}
`
	stdout.Reset()
	stderr.Reset()
	if code := runOneCollectorMCPWithInput([]string{"--profile-file", path}, strings.NewReader(messages), &stdout, &stderr); code != 0 {
		t.Fatal(code, stderr.String())
	}
	decoder := json.NewDecoder(&stdout)
	var results []map[string]json.RawMessage
	for decoder.More() {
		var result map[string]json.RawMessage
		if err := decoder.Decode(&result); err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	if len(results) != 8 {
		t.Fatal("unexpected response count", len(results))
	}
	if !bytes.Contains(results[3]["result"], []byte(`validated`)) || !bytes.Contains(results[4]["result"], []byte(`"isError":true`)) || !bytes.Contains(results[5]["result"], []byte(`"isError":true`)) || !bytes.Contains(results[6]["error"], []byte(`-32602`)) {
		t.Fatal("MCP validation/result framing wrong")
	}
	if !bytes.Contains(results[7]["result"], []byte(`disabled`)) || !bytes.Contains(results[7]["result"], []byte(`"isError":true`)) {
		t.Fatal("disabled tool upload must report an error")
	}
}
