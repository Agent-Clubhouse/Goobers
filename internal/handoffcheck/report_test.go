package handoffcheck

import (
	"context"
	"reflect"
	"testing"
)

func TestReportBoolTriState(t *testing.T) {
	tests := []struct {
		in        InputValid
		valid, ok bool
	}{
		{InputValidTrue, true, true},
		{InputValidFalse, false, true},
		{InputValidUnknown, false, false},
		{"", false, false},
		{"bogus", false, false},
	}
	for _, tc := range tests {
		valid, known := Report{InputValid: tc.in}.Bool()
		if valid != tc.valid || known != tc.ok {
			t.Errorf("Bool(%q) = %v,%v want %v,%v", tc.in, valid, known, tc.valid, tc.ok)
		}
	}
}

func TestReportFromOutputs(t *testing.T) {
	want := Report{InputValid: InputValidFalse, Error: "boom", Entries: []ReportEntry{{Input: "a", Valid: false}}}
	var nilPtr *Report
	tests := []struct {
		name    string
		outputs map[string]interface{}
		want    Report
		ok      bool
	}{
		{"nil outputs", nil, Report{}, false},
		{"empty outputs", map[string]interface{}{}, Report{}, false},
		{"missing key", map[string]interface{}{"x": 1}, Report{}, false},
		{"nil value", map[string]interface{}{OutputKey: nil}, Report{}, false},
		{"value", map[string]interface{}{OutputKey: want}, want, true},
		{"pointer", map[string]interface{}{OutputKey: &want}, want, true},
		{"nil pointer", map[string]interface{}{OutputKey: nilPtr}, Report{}, false},
		{"journal map", map[string]interface{}{OutputKey: map[string]interface{}{
			"inputValid": "false", "error": "boom",
			"entries": []interface{}{map[string]interface{}{"input": "a", "valid": false}},
		}}, want, true},
		{"malformed type", map[string]interface{}{OutputKey: "not a report"}, Report{}, false},
		{"malformed field", map[string]interface{}{OutputKey: map[string]interface{}{"entries": "x"}}, Report{}, false},
		{"unmarshalable", map[string]interface{}{OutputKey: make(chan int)}, Report{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ReportFromOutputs(tc.outputs)
			if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v,%v want %+v,%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestWithReportRoundTrip(t *testing.T) {
	if _, ok := ReportFromContext(context.Background()); ok {
		t.Fatal("unexpected report on empty context")
	}
	//nolint:staticcheck // exercising nil-context guard
	if _, ok := ReportFromContext(nil); ok {
		t.Fatal("unexpected report on nil context")
	}
	want := Report{InputValid: InputValidTrue}
	got, ok := ReportFromContext(WithReport(context.Background(), want))
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v,%v", got, ok)
	}
}

func TestWithPublicationSchemasRoundTrip(t *testing.T) {
	if got := PublicationSchemasFromContext(context.Background()); got != nil {
		t.Fatalf("got %v want nil", got)
	}
	//nolint:staticcheck // exercising nil-context guard
	if got := PublicationSchemasFromContext(nil); got != nil {
		t.Fatalf("got %v want nil", got)
	}
	base := context.Background()
	if WithPublicationSchemas(base, nil) != base || WithPublicationSchemas(base, map[string]*Schema{}) != base {
		t.Fatal("empty schemas should return ctx unchanged")
	}
	s := &Schema{}
	in := map[string]*Schema{"slot": s}
	ctx := WithPublicationSchemas(base, in)
	in["other"] = &Schema{}
	got := PublicationSchemasFromContext(ctx)
	if len(got) != 1 || got["slot"] != s {
		t.Fatalf("got %v", got)
	}
}
