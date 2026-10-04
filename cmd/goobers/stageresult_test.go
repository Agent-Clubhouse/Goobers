package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestStageResultJSONFormats(t *testing.T) {
	tests := []struct {
		name string
		opts stageResultOptions
		want string
	}{
		{
			name: "compact without newline",
			opts: stageResultOptions{MarshalLabel: "marshal result"},
			want: `{"name":"goobers","count":2}`,
		},
		{
			name: "indented with newline",
			opts: stageResultOptions{
				MarshalLabel:    "marshal result",
				Indented:        true,
				TrailingNewline: true,
			},
			want: "{\n  \"name\": \"goobers\",\n  \"count\": 2\n}\n",
		},
	}
	value := struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}{Name: "goobers", Count: 2}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			var stderr bytes.Buffer
			if code := writeStageResultJSON(&stderr, path, value, test.opts); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != test.want {
				t.Fatalf("result bytes = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStageResultJSONMarshalError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	var stderr bytes.Buffer
	code := writeStageResultJSON(&stderr, path, make(chan int), stageResultOptions{
		MarshalLabel: "marshal stage fixture",
	})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	const want = "error: marshal stage fixture: json: unsupported type: chan int\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("result file exists after marshal error: %v", err)
	}
}

func TestStageResultJSONWriteErrorExitCodes(t *testing.T) {
	for _, wantCode := range []int{1, 2} {
		t.Run(fmt.Sprintf("exit %d", wantCode), func(t *testing.T) {
			path := t.TempDir()
			directErr := os.WriteFile(path, []byte(`{"ok":true}`), 0o644)
			if directErr == nil {
				t.Fatal("writing to a directory unexpectedly succeeded")
			}
			var stderr bytes.Buffer
			code := writeStageResultJSON(&stderr, path, map[string]bool{"ok": true}, stageResultOptions{
				MarshalLabel:       "marshal stage fixture",
				WriteLabel:         "write stage fixture",
				WriteErrorExitCode: wantCode,
			})
			if code != wantCode {
				t.Fatalf("exit code = %d, want %d", code, wantCode)
			}
			wantError := fmt.Sprintf("error: write stage fixture %s: %v\n", path, directErr)
			if stderr.String() != wantError {
				t.Fatalf("stderr = %q, want %q", stderr.String(), wantError)
			}
		})
	}
}
