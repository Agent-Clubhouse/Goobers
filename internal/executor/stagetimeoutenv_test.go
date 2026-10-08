package executor

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestRunContextEnvCarriesEffectiveStageTimeout is #5572: a goobers CLI stage
// must learn the deadline the executor actually enforces, whichever surface
// supplied it, not only a declared inputs.timeout.
func TestRunContextEnvCarriesEffectiveStageTimeout(t *testing.T) {
	cases := []struct {
		name           string
		defaultTimeout time.Duration
		env            apiv1.InvocationEnvelope
		want           string
	}{
		{
			name: "limits.maxDurationSeconds",
			env: apiv1.InvocationEnvelope{
				Limits: apiv1.Limits{MaxDurationSeconds: 300},
				Inputs: map[string]interface{}{InputTimeout: "20m"},
			},
			want: "5m0s",
		},
		{
			name: "inputs.timeout",
			env:  apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputTimeout: "7m"}},
			want: "7m0s",
		},
		{name: "runner default", defaultTimeout: 4 * time.Minute, want: "4m0s"},
		{name: "built-in default", want: DefaultTimeout.String()},
		{
			name: "immediate deadline is not advertised",
			env:  apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputTimeout: "0s"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := &ShellExecutor{DefaultTimeout: tc.defaultTimeout}
			tc.env.RunID, tc.env.TaskID = "run-1", "run-1:pr-select"
			got, found := "", false
			for _, kv := range exec.runContextEnv(t.Context(), tc.env) {
				if value, ok := strings.CutPrefix(kv, StageTimeoutEnvVar+"="); ok {
					got, found = value, true
				}
			}
			if tc.want == "" {
				if found {
					t.Fatalf("%s = %q, want unset", StageTimeoutEnvVar, got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("%s = %q (found=%v), want %q", StageTimeoutEnvVar, got, found, tc.want)
			}
		})
	}
}
