package onecollector

import (
	"encoding/json"
	"strings"
	"testing"
)

func syntheticInputProfile() Profile {
	return Profile{ProfileVersion: 1, EnableEnv: "SYNTHETIC_UPLOAD", Endpoint: "https://collector.example.invalid",
		RoutingKey: KeyRef{Env: "SYNTHETIC_ROUTE"}, InstrumentationKey: KeyRef{Env: "SYNTHETIC_KEY"},
		Inputs: []Input{{Name: "synthetic", NormalizeKeys: true,
			Schema:      json.RawMessage(`{"type":"object","additionalProperties":false,"required":["displayName","enabled","details","sequence"],"properties":{"displayName":{"type":"string"},"enabled":{"type":"boolean"},"details":{"type":"object"},"sequence":{"type":"integer"},"optional":{"type":"string"}}}`),
			Defaults:    map[string]json.RawMessage{"enabled": json.RawMessage("false"), "details": json.RawMessage("{}")},
			RemoveBlank: []string{"optional"}, SerializeFields: []string{"details"},
			Rules: []string{`unsignedDecimal(tokens["sequence"]) && uint(input.sequence) == 18446744073709551615u`, `utf16Length(input.displayName) <= 8`}}},
		Mappings: []Mapping{{Signal: "input", Match: map[string][]string{"input_name": {"synthetic"}}, EventName: "Synthetic.Input", EnvelopeVersion: "4.0", Fields: map[string]Field{"name": {Source: "input.displayName"}, "details": {Source: "input.details"}, "sequence": {Source: "input.sequence"}, "enabled": {Source: "input.enabled"}}}},
		Tools: []Tool{{Name: "synthetic_validate", Input: "synthetic", PayloadPath: "event", ValidationField: "check",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["event"],"properties":{"event":{"type":"object"},"check":{"type":"boolean"}}}`)}},
	}
}

func TestExplicitInputNormalizeDefaultsExactNumbers(t *testing.T) {
	profile := syntheticInputProfile()
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	envelopes, err := profile.PrepareInput("synthetic", []byte(`{"Display_Name":"hello","Sequence":18446744073709551615,"optional":" "}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Serialize(envelopes)
	if err != nil || !strings.Contains(string(raw), "18446744073709551615") || !strings.Contains(string(raw), `"details":"{}"`) || !strings.Contains(string(raw), `"enabled":false`) {
		t.Fatal(string(raw), err)
	}
	for _, bad := range []string{
		`{"Display_Name":"hello","displayName":"other","Sequence":18446744073709551615}`,
		`{"Display_Name":"hello","Sequence":18446744073709551615,"enabled":null}`,
		`{"Display_Name":"hello","Sequence":18446744073709551615,"unknown":1}`,
		`{"Display_Name":"hello","Sequence":18446744073709551615,"Sequence":1}`,
		`{"Display_Name":"hello","Sequence":1.8446744073709551615e19}`,
		`{"Display_Name":"hello","Sequence":18446744073709551615,"details":null}`,
	} {
		if _, err := profile.PrepareInput("synthetic", []byte(bad)); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	if got, err := NormalizeKey("my URL"); err != nil || got != "myUrl" {
		t.Fatal(got, err)
	}
	if got, err := NormalizeKey("ABCName"); err != nil || got != "abcName" {
		t.Fatal(got, err)
	}
}

func TestExplicitInputNestedBatchAndCELHelpers(t *testing.T) {
	profile := syntheticInputProfile()
	profile.Inputs = []Input{{Name: "batch", ItemsPath: "$", Schema: json.RawMessage(`{"type":"array","minItems":1,"maxItems":1000,"items":{"type":"object"}}`),
		RecordSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["body"],"properties":{"body":{"type":"string","contentMediaType":"application/json","contentSchema":{"type":"object","required":["n","time"],"properties":{"n":{"type":"string"},"time":{"type":"string"}}}}}}`),
		RecordRules:  []string{`unsignedDecimal(jsonParse(input.body)["n"])`, `jsonDepth(jsonParse(input.body)) <= 10`, `jsonObjects(jsonParse(input.body)).all(o, !("data" in o) || canonicalBase64(o.data))`, `input.body.substring(0,1) == "{"`}}}
	profile.Tools = nil
	profile.Mappings = []Mapping{{Signal: "input", Match: map[string][]string{"input_name": {"batch"}}, EventName: "Synthetic.Batch", EnvelopeVersion: "4.0", DataSource: "input_json.body", TimestampSource: "input_json.body.time", TimestampFormat: "unix_nano"}}
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	body := `[{"body":"{ \"n\":\"18446744073709551615\", \"time\":\"1700000000000000000\", \"data\":\"YQ==\" }"}]`
	envelopes, err := profile.PrepareInput("batch", []byte(body))
	if err != nil || len(envelopes) != 1 || envelopes[0].Time != "2023-11-14T22:13:20Z" {
		t.Fatal(envelopes, err)
	}
	for _, bad := range []string{strings.Replace(body, "YQ==", "YQ", 1), strings.Replace(body, `\"time\"`, `\"bad\"`, 1), strings.Replace(body, `\"n\"`, `\"n\": \"1\", \"n\"`, 1)} {
		if _, err := profile.PrepareInput("batch", []byte(bad)); err == nil {
			t.Fatal("invalid embedded input accepted")
		}
	}
	if err := evaluateRules([]string{`jsonParse(input.text, false)["a.b"] == 1`}, map[string]any{"text": `{"a.b":1}`}); err != nil {
		t.Fatal(err)
	}
	if err := evaluateRules([]string{`jsonParse(input.text, true) != null`}, map[string]any{"text": `{"my_key":1,"myKey":2}`}); err == nil {
		t.Fatal("nested collision accepted")
	}
}

func TestExplicitToolValidationAndRemoteSchemaRef(t *testing.T) {
	profile := syntheticInputProfile()
	input, raw, validate, err := profile.ToolPayload("synthetic_validate", json.RawMessage(`{"check":true,"event":{"Display_Name":"hello","Sequence":18446744073709551615}}`))
	if err != nil || input != "synthetic" || !validate {
		t.Fatal(input, validate, err)
	}
	if _, err := profile.PrepareInput(input, raw); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := profile.ToolPayload("synthetic_validate", json.RawMessage(`{"check":"true","event":{}}`)); err == nil {
		t.Fatal("non-Boolean validation accepted")
	}
	if _, err := compileSchema(json.RawMessage(`{"$ref":"https://example.invalid/schema"}`)); err == nil {
		t.Fatal("external schema reference accepted")
	}
}

func TestExplicitSerializedFieldPreservesHTMLAndNumberTokens(t *testing.T) {
	profile := syntheticInputProfile()
	envelopes, err := profile.PrepareInput("synthetic", []byte(`{"displayName":"hello","sequence":18446744073709551615,"details":{"marker":"x<y&z","n":9007199254740993}}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Serialize(envelopes)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Data struct {
			Details string `json:"details"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Data.Details != `{"marker":"x<y&z","n":9007199254740993}` {
		t.Fatal(record.Data.Details)
	}
	if err := evaluateRules([]string{`jsonStringify(input) == '{"marker":"x<y&z"}'`}, map[string]any{"marker": "x<y&z"}); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitCollisionCheckingUsesUnicodeWithoutRewriting(t *testing.T) {
	value, err := DecodeObject(`{"items":[],"\u00c9Key":1,"\u00e9Key":2}`, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := normalizeValue(value, false); err == nil {
		t.Fatal("Unicode normalized collision accepted")
	}
	value, err = DecodeObject(`{"items":[],"\u00c9Key":1}`, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := normalizeValue(value, false); err != nil {
		t.Fatal(err)
	}
	if _, present := value["\u00c9Key"]; !present {
		t.Fatal("collision checking rewrote the caller's key")
	}
	if _, err := NormalizeKey("\u00c9Key"); err == nil {
		t.Fatal("rewrite mode accepted a non-ASCII output key")
	}
}

func TestExplicitTokenByteBudgetUsesCompactJSONAndOriginalNumbers(t *testing.T) {
	profile := syntheticInputProfile()
	profile.Inputs[0].Rules = []string{`size(bytes(tokens["details"])) <= 2097152`, `tokens["details"].contains("9.007199254740993e15")`}
	raw := `{"displayName":"hello","sequence":18446744073709551615,"details":{"marker":"` + strings.Repeat("<", 360000) + `","n":9.007199254740993e15}}`
	envelopes, err := profile.PrepareInput("synthetic", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Serialize(envelopes)
	if err != nil || len(encoded) > 400000 {
		t.Fatal("compact token budget or numeric spelling changed", len(encoded), err)
	}
}
