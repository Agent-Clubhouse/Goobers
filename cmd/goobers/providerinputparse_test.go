package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
)

func TestParseIntInput(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int
		wantErr string
	}{
		{name: "lower boundary", raw: "1", want: 1},
		{name: "upper boundary", raw: "3", want: 3},
		{name: "empty value", raw: "", wantErr: `invalid count ""`},
		{name: "invalid syntax", raw: "two", wantErr: `invalid count "two"`},
		{name: "untrimmed value is a syntax error", raw: " 2 ", wantErr: `invalid count " 2 "`},
		{name: "below range", raw: "0", wantErr: `invalid count "0"`},
		{name: "above range", raw: "4", wantErr: `invalid count "4"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIntInput(
				tt.raw,
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

// TestParseIntInputReadsDefaultThroughProviderInput pins the call-site shape
// every migrated command uses: the literal providerInput read supplies the
// default when the input is unset or empty, and the helper only parses.
func TestParseIntInputReadsDefaultThroughProviderInput(t *testing.T) {
	validate := func(value int) bool { return value >= 1 }
	errorText := func(raw string, _ error) string { return fmt.Sprintf("invalid count %q", raw) }
	for _, tt := range []struct {
		name string
		set  string
		want int
	}{
		{name: "empty uses default", set: "", want: 2},
		{name: "set overrides default", set: "5", want: 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(executor.InputEnvVar("count"), tt.set)
			got, err := parseIntInput(providerInput("count", "2"), validate, errorText)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tt.want {
				t.Fatalf("value = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseFloatInput(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    float64
		wantErr string
	}{
		{name: "lower boundary", raw: "0", want: 0},
		{name: "upper boundary", raw: "0.99", want: 0.99},
		{name: "invalid syntax", raw: "half", wantErr: `invalid ratio "half"`},
		{name: "below range", raw: "-0.1", wantErr: `invalid ratio "-0.1"`},
		{name: "above range", raw: "1", wantErr: `invalid ratio "1"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFloatInput(
				tt.raw,
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

func TestParseDurationInput(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr string
	}{
		{name: "valid duration", raw: "90m", want: 90 * time.Minute},
		{name: "smallest positive duration", raw: "1ns", want: time.Nanosecond},
		{name: "zero is below range", raw: "0s", wantErr: `invalid duration "0s"`},
		{name: "negative is below range", raw: "-1h", wantErr: `invalid duration "-1h"`},
		{name: "invalid syntax", raw: "soon", wantErr: `invalid duration "soon"`},
		{name: "bare number is a syntax error", raw: "60", wantErr: `invalid duration "60"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDurationInput(
				tt.raw,
				func(value time.Duration) bool { return value > 0 },
				func(raw string, _ error) string { return fmt.Sprintf("invalid duration %q", raw) },
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

func TestParseBoolInput(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    bool
		wantErr string
	}{
		{name: "true", raw: "true", want: true},
		{name: "false", raw: "false"},
		{name: "ParseBool spelling", raw: "1", want: true},
		{name: "invalid syntax", raw: "sometimes", wantErr: `invalid enabled "sometimes": strconv.ParseBool: parsing "sometimes": invalid syntax`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBoolInput(
				tt.raw,
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

// TestReadBacklogStalenessPolicyStrictAutoClose pins the exact true/false
// contract staleAutoClose had before it moved onto parseBoolInput: spellings
// strconv.ParseBool accepts ("1", "TRUE", "t") must still be rejected with
// the historical error text.
func TestReadBacklogStalenessPolicyStrictAutoClose(t *testing.T) {
	tests := []struct {
		raw     string
		want    bool
		wantErr string
	}{
		{raw: "", want: false},
		{raw: "true", want: true},
		{raw: "false", want: false},
		{raw: " true ", want: true},
		{raw: "1", wantErr: `invalid staleAutoClose "1" (want true or false)`},
		{raw: "TRUE", wantErr: `invalid staleAutoClose "TRUE" (want true or false)`},
		{raw: "t", wantErr: `invalid staleAutoClose "t" (want true or false)`},
		{raw: "yes", wantErr: `invalid staleAutoClose "yes" (want true or false)`},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.raw), func(t *testing.T) {
			t.Setenv(executor.InputEnvVar("staleAfterDays"), "")
			t.Setenv(executor.InputEnvVar("staleAutoClose"), tt.raw)
			policy, err := readBacklogStalenessPolicy()
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readBacklogStalenessPolicy: %v", err)
			}
			if policy.autoCloseStale != tt.want {
				t.Fatalf("autoCloseStale = %t, want %t", policy.autoCloseStale, tt.want)
			}
		})
	}
}
