package harness

import (
	"errors"
	"testing"
)

func TestExtractCompletionJSONToleratesFencesAndProse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "already bare",
			in:   `{"status":"success"}`,
			want: `{"status":"success"}`,
		},
		{
			name: "trims surrounding whitespace",
			in:   "\n  {\"status\":\"success\"}  \n",
			want: `{"status":"success"}`,
		},
		{
			name: "fenced with json language tag",
			in:   "```json\n{\"status\":\"success\"}\n```",
			want: `{"status":"success"}`,
		},
		{
			name: "fenced without language tag",
			in:   "```\n{\"status\":\"success\"}\n```",
			want: `{"status":"success"}`,
		},
		{
			name: "fenced with prose after the block",
			in:   "```json\n{\"status\":\"success\"}\n```\nDone.",
			want: `{"status":"success"}`,
		},
		{
			name: "prose preamble then object",
			in:   "Here is the result:\n{\"status\":\"success\"}",
			want: `{"status":"success"}`,
		},
		{
			name: "object with nested braces in strings",
			in:   "prefix {\"summary\":\"uses {braces} inside\",\"status\":\"success\"} suffix",
			want: `{"summary":"uses {braces} inside","status":"success"}`,
		},
		{
			name: "top-level array",
			in:   "```json\n[{\"a\":1}]\n```",
			want: `[{"a":1}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(extractCompletionJSON([]byte(tc.in)))
			if got != tc.want {
				t.Fatalf("extractCompletionJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtractCompletionJSONLeavesUnrecoverableInputUnchanged(t *testing.T) {
	in := "no json here at all"
	if got := string(extractCompletionJSON([]byte(in))); got != in {
		t.Fatalf("extractCompletionJSON(%q) = %q, want unchanged", in, got)
	}
}

func TestReadCopilotResponseCompletionAcceptsFencedEnvelope(t *testing.T) {
	capture := newTranscriptBuffer(1 << 16)
	_, _ = capture.Write([]byte("```json\n{\"status\":\"success\",\"outputs\":{},\"summary\":\"done\",\"metrics\":{}}\n```"))

	payload, err := readCopilotResponseCompletion(ModeInvoke, capture)
	if err != nil {
		t.Fatalf("readCopilotResponseCompletion returned error: %v", err)
	}
	if got, want := string(payload), `{"status":"success","outputs":{},"summary":"done","metrics":{}}`; got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

func TestReadCopilotResponseCompletionStillRejectsNonJSON(t *testing.T) {
	capture := newTranscriptBuffer(1 << 16)
	_, _ = capture.Write([]byte("I was unable to complete the task."))

	_, err := readCopilotResponseCompletion(ModeInvoke, capture)
	if !errors.Is(err, ErrNoCompletion) {
		t.Fatalf("error = %v, want ErrNoCompletion", err)
	}
}
