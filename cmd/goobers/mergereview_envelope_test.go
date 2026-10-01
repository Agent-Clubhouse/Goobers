package main

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/executor"
)

func TestReadSelectedPREnvelope(t *testing.T) {
	tests := []struct {
		name       string
		source     selectedPREnvelopeSource
		inputs     map[string]string
		want       selectedPREnvelope
		wantStderr string
	}{
		{
			name:       "elect-lander missing selected number names gather sibling context",
			source:     electLanderEnvelopeSource,
			wantStderr: "error: selectedNumber is required (inputsFrom gather-sibling-context's selectedNumber output)\n",
		},
		{
			name:       "apply-verdict missing selected number names pr-select",
			source:     applyVerdictEnvelopeSource,
			wantStderr: "error: selectedNumber is required (inputsFrom pr-select's number output)\n",
		},
		{
			name:   "invalid selected number",
			source: electLanderEnvelopeSource,
			inputs: map[string]string{"selectedNumber": "not-a-number"},
			wantStderr: "error: invalid selectedNumber \"not-a-number\": " +
				"strconv.Atoi: parsing \"not-a-number\": invalid syntax\n",
		},
		{
			name:   "missing head SHA",
			source: applyVerdictEnvelopeSource,
			inputs: map[string]string{"selectedNumber": "42"},
			wantStderr: "error: selectedHeadSha is required " +
				"(inputsFrom gather-sibling-context's deterministic output)\n",
		},
		{
			name:   "missing base SHA",
			source: applyVerdictEnvelopeSource,
			inputs: map[string]string{
				"selectedNumber":  "42",
				"selectedHeadSha": "head",
			},
			wantStderr: "error: selectedBaseSha is required " +
				"(inputsFrom gather-sibling-context's deterministic output)\n",
		},
		{
			name:   "invalid advisory mode",
			source: applyVerdictEnvelopeSource,
			inputs: map[string]string{
				"selectedNumber":  "42",
				"selectedHeadSha": "head",
				"selectedBaseSha": "base",
				"advisoryMode":    "sometimes",
			},
			wantStderr: "error: invalid advisoryMode input: " +
				"strconv.ParseBool: parsing \"sometimes\": invalid syntax\n",
		},
		{
			name:   "scope gate state passes through",
			source: applyVerdictEnvelopeSource,
			inputs: map[string]string{
				"selectedNumber":  "042",
				"selectedHeadSha": "head",
				"selectedBaseSha": "base",
				"advisoryMode":    "true",
				"scopeGateParked": "parked-verbatim",
			},
			want: selectedPREnvelope{
				Number:          42,
				NumberString:    "042",
				HeadSHA:         "head",
				BaseSHA:         "base",
				Advisory:        true,
				ScopeGateParked: "parked-verbatim",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{
				"selectedNumber",
				"selectedHeadSha",
				"selectedBaseSha",
				"advisoryMode",
				"scopeGateParked",
			} {
				t.Setenv(executor.InputEnvVar(name), "")
			}
			for name, value := range tt.inputs {
				t.Setenv(executor.InputEnvVar(name), value)
			}

			var stderr bytes.Buffer
			got, code, ok := readSelectedPREnvelope(&stderr, tt.source)
			if tt.wantStderr != "" {
				if ok || code != 1 {
					t.Fatalf("readSelectedPREnvelope = (%+v, %d, %v), want failure code 1", got, code, ok)
				}
				if stderr.String() != tt.wantStderr {
					t.Fatalf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
				}
				return
			}
			if !ok || code != 0 {
				t.Fatalf("readSelectedPREnvelope = (%+v, %d, %v), stderr = %q", got, code, ok, stderr.String())
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("envelope = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSelectedPREnvelopeBaseResult(t *testing.T) {
	envelope := selectedPREnvelope{
		Number:          42,
		NumberString:    "042",
		HeadSHA:         "head",
		BaseSHA:         "base",
		Advisory:        true,
		ScopeGateParked: "true",
	}
	want := map[string]string{
		"selectedNumber":  "42",
		"selectedHeadSha": "head",
		"selectedBaseSha": "base",
		"advisoryMode":    "true",
		"scopeGateParked": "true",
	}
	if got := envelope.baseResult(); !reflect.DeepEqual(got, want) {
		t.Fatalf("baseResult() = %v, want %v", got, want)
	}
}
