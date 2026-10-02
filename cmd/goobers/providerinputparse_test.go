package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
)

func TestProviderInputInt(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		raw     string
		def     string
		trim    bool
		want    int
		wantErr string
	}{
		{name: "missing value uses default", def: "2", want: 2},
		{name: "empty value uses default", set: true, def: "2", want: 2},
		{name: "lower boundary", set: true, raw: "1", def: "2", want: 1},
		{name: "upper boundary", set: true, raw: "3", def: "2", want: 3},
		{name: "trimmed value", set: true, raw: " 2 ", trim: true, want: 2},
		{name: "invalid syntax", set: true, raw: "two", wantErr: `invalid count "two"`},
		{name: "below range", set: true, raw: "0", wantErr: `invalid count "0"`},
		{name: "above range", set: true, raw: "4", wantErr: `invalid count "4"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(executor.InputEnvVar("count"), "")
			if tt.set {
				t.Setenv(executor.InputEnvVar("count"), tt.raw)
			}
			got, err := parseProviderIntInput(
				"count",
				tt.def,
				tt.trim,
				func(value int) bool { return value >= 1 && value <= 3 },
				func(raw string, _ error) string { return fmt.Sprintf("invalid count %q", raw) },
			)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tt.want {
				t.Fatalf("value = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestProviderInputFloat(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		raw     string
		def     string
		want    float64
		wantErr string
	}{
		{name: "missing value uses default", def: "0.5", want: 0.5},
		{name: "lower boundary", set: true, raw: "0", want: 0},
		{name: "upper boundary", set: true, raw: "0.99", want: 0.99},
		{name: "invalid syntax", set: true, raw: "half", wantErr: `invalid ratio "half"`},
		{name: "below range", set: true, raw: "-0.1", wantErr: `invalid ratio "-0.1"`},
		{name: "above range", set: true, raw: "1", wantErr: `invalid ratio "1"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(executor.InputEnvVar("ratio"), "")
			if tt.set {
				t.Setenv(executor.InputEnvVar("ratio"), tt.raw)
			}
			got, err := parseProviderFloatInput(
				"ratio",
				tt.def,
				false,
				func(value float64) bool { return value >= 0 && value < 1 },
				func(raw string, _ error) string { return fmt.Sprintf("invalid ratio %q", raw) },
			)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tt.want {
				t.Fatalf("value = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProviderInputDuration(t *testing.T) {
	t.Setenv(executor.InputEnvVar("duration"), "0s")
	_, err := parseProviderDurationInput(
		"duration",
		"1h",
		false,
		func(value time.Duration) bool { return value > 0 },
		func(raw string, _ error) string { return fmt.Sprintf("invalid duration %q", raw) },
	)
	if err == nil || err.Error() != `invalid duration "0s"` {
		t.Fatalf(`error = %v, want invalid duration "0s"`, err)
	}
}

func TestProviderInputBool(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		raw     string
		want    bool
		wantErr string
	}{
		{name: "missing value uses default", want: true},
		{name: "true", set: true, raw: "true", want: true},
		{name: "false", set: true, raw: "false"},
		{name: "invalid syntax", set: true, raw: "sometimes", wantErr: `invalid enabled "sometimes": strconv.ParseBool: parsing "sometimes": invalid syntax`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(executor.InputEnvVar("enabled"), "")
			if tt.set {
				t.Setenv(executor.InputEnvVar("enabled"), tt.raw)
			}
			got, err := parseProviderBoolInput(
				"enabled",
				"true",
				false,
				func(_ string, _ bool) bool { return true },
				func(raw string, parseErr error) string {
					return fmt.Sprintf("invalid enabled %q: %v", raw, parseErr)
				},
			)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tt.want {
				t.Fatalf("value = %t, want %t", got, tt.want)
			}
		})
	}
}
