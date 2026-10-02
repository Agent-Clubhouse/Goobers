package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goobers/goobers/internal/executor"
)

func TestSelectedPREnvelopeCommandBehavior(t *testing.T) {
	tests := []struct {
		name       string
		command    func([]string, *bytes.Buffer, *bytes.Buffer) int
		inputs     map[string]string
		wantCode   int
		wantStdout string
		wantStderr string
		wantResult map[string]string
	}{
		{
			name:     "elect-lander missing selected number names gather sibling context",
			command:  runElectLanderForEnvelopeTest,
			wantCode: 1,
			wantStderr: "error: selectedNumber is required " +
				"(inputsFrom gather-sibling-context's selectedNumber output)\n",
		},
		{
			name:     "apply-verdict missing selected number names pr-select",
			command:  runApplyVerdictForEnvelopeTest,
			wantCode: 1,
			wantStderr: "error: selectedNumber is required " +
				"(inputsFrom pr-select's number output)\n",
		},
		{
			name:     "invalid selected number",
			command:  runElectLanderForEnvelopeTest,
			inputs:   map[string]string{"selectedNumber": "not-a-number"},
			wantCode: 1,
			wantStderr: "error: invalid selectedNumber \"not-a-number\": " +
				"strconv.Atoi: parsing \"not-a-number\": invalid syntax\n",
		},
		{
			name:     "missing head SHA",
			command:  runApplyVerdictForEnvelopeTest,
			inputs:   map[string]string{"selectedNumber": "42"},
			wantCode: 1,
			wantStderr: "error: selectedHeadSha is required " +
				"(inputsFrom gather-sibling-context's deterministic output)\n",
		},
		{
			name:    "missing base SHA",
			command: runApplyVerdictForEnvelopeTest,
			inputs: map[string]string{
				"selectedNumber":  "42",
				"selectedHeadSha": "head",
			},
			wantCode: 1,
			wantStderr: "error: selectedBaseSha is required " +
				"(inputsFrom gather-sibling-context's deterministic output)\n",
		},
		{
			name:    "invalid advisory mode",
			command: runApplyVerdictForEnvelopeTest,
			inputs: map[string]string{
				"selectedNumber":  "42",
				"selectedHeadSha": "head",
				"selectedBaseSha": "base",
				"advisoryMode":    "sometimes",
			},
			wantCode: 1,
			wantStderr: "error: invalid advisoryMode input: " +
				"strconv.ParseBool: parsing \"sometimes\": invalid syntax\n",
		},
		{
			name:    "scope gate and complete base result pass through",
			command: runElectLanderForEnvelopeTest,
			inputs: map[string]string{
				"selectedNumber":      "042",
				"selectedHeadSha":     "head",
				"selectedBaseSha":     "base",
				"advisoryMode":        "true",
				"scopeGateParked":     "parked-verbatim",
				"reviewDigest":        "digest",
				"overlappingSiblings": "43,44",
				"unlandableSiblings":  "45",
			},
			wantStdout: "PR #42 is advisory-only — skipping lander election\n",
			wantResult: map[string]string{
				"elected":                "false",
				"selectedNumber":         "42",
				"selectedHeadSha":        "head",
				"selectedBaseSha":        "base",
				"reviewDigest":           "digest",
				"overlappingSiblingsCsv": "43,44",
				"unlandableSiblingsCsv":  "45",
				"advisoryMode":           "true",
				"scopeGateParked":        "parked-verbatim",
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
				"reviewDigest",
				"overlappingSiblings",
				"unlandableSiblings",
				"resultFile",
			} {
				t.Setenv(executor.InputEnvVar(name), "")
			}
			for name, value := range tt.inputs {
				t.Setenv(executor.InputEnvVar(name), value)
			}

			resultFile := filepath.Join(t.TempDir(), "result.json")
			t.Setenv(executor.InputEnvVar("resultFile"), resultFile)
			var stdout, stderr bytes.Buffer
			code := tt.command([]string{t.TempDir()}, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d; stdout = %q, stderr = %q", code, tt.wantCode, stdout.String(), stderr.String())
			}
			if stdout.String() != tt.wantStdout {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if stderr.String() != tt.wantStderr {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
			}
			if tt.wantResult == nil {
				return
			}
			data, err := os.ReadFile(resultFile)
			if err != nil {
				t.Fatalf("read result: %v", err)
			}
			var got map[string]string
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			if !reflect.DeepEqual(got, tt.wantResult) {
				t.Fatalf("result = %v, want %v", got, tt.wantResult)
			}
		})
	}
}

func runElectLanderForEnvelopeTest(args []string, stdout, stderr *bytes.Buffer) int {
	return runElectLander(args, stdout, stderr)
}

func runApplyVerdictForEnvelopeTest(args []string, stdout, stderr *bytes.Buffer) int {
	return runApplyVerdict(args, stdout, stderr)
}
