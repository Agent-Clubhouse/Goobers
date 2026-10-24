package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHANightlyAssemblyClassificationCases(t *testing.T) {
	root := moduleRoot(t)
	for _, test := range []struct {
		name       string
		input      string
		wantPass   bool
		wantReady  string
		wantOutput string
	}{
		{
			name:     "unimplemented seam warns",
			input:    "Claims\tpending\thttps://github.com/Agent-Clubhouse/Goobers/issues/7808\n",
			wantPass: true, wantReady: "ready=false", wantOutput: "cloud reference blocked",
		},
		{
			name:       "missing deployed flag fails",
			input:      "Journal\tpending\thttps://github.com/Agent-Clubhouse/Goobers/issues/7818\tneeds --object-store-url\n",
			wantOutput: "cloud assembly configuration drift",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			outputFile, summaryFile := filepath.Join(directory, "output"), filepath.Join(directory, "summary")
			cmd := exec.Command("go", "run", "./test/ha/kind/cmd/assemblyclassify", "1")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+outputFile, "GITHUB_STEP_SUMMARY="+summaryFile)
			cmd.Stdin = strings.NewReader(test.input)
			output, err := cmd.CombinedOutput()
			if (err == nil) != test.wantPass {
				t.Fatalf("status error = %v, output = %s", err, output)
			}
			ready, _ := os.ReadFile(outputFile)
			if test.wantReady != "" && !strings.Contains(string(ready), test.wantReady) {
				t.Fatalf("GITHUB_OUTPUT = %q, want %q", ready, test.wantReady)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("output = %q, want %q", output, test.wantOutput)
			}
		})
	}
}
