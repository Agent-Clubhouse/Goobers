package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

func TestParseOptionalRoot(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantRoot   string
		wantOK     bool
		wantOutput string
	}{
		{
			name:       "parse error",
			args:       []string{"--unknown"},
			wantOutput: "flag provided but not defined: -unknown\ncommand-specific usage\n",
		},
		{
			name:     "zero args",
			wantRoot: ".",
			wantOK:   true,
		},
		{
			name:     "one arg",
			args:     []string{"instance"},
			wantRoot: "instance",
			wantOK:   true,
		},
		{
			name:       "two args",
			args:       []string{"first", "second"},
			wantOutput: "command-specific usage\n",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var output strings.Builder
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(&output)
			fs.Usage = func() {
				_, _ = io.WriteString(&output, "command-specific usage\n")
			}

			root, ok := parseOptionalRoot(fs, testCase.args)

			if root != testCase.wantRoot {
				t.Errorf("root = %q, want %q", root, testCase.wantRoot)
			}
			if ok != testCase.wantOK {
				t.Errorf("ok = %t, want %t", ok, testCase.wantOK)
			}
			if got := output.String(); got != testCase.wantOutput {
				t.Errorf("output = %q, want %q", got, testCase.wantOutput)
			}
		})
	}
}

func TestParseRequiredArgOptionalRoot(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantArg    string
		wantRoot   string
		wantOK     bool
		wantOutput string
	}{
		{
			name:       "missing required arg",
			wantOutput: "command-specific usage\n",
		},
		{
			name:     "required only",
			args:     []string{"required"},
			wantArg:  "required",
			wantRoot: ".",
			wantOK:   true,
		},
		{
			name:     "required plus root",
			args:     []string{"required", "instance"},
			wantArg:  "required",
			wantRoot: "instance",
			wantOK:   true,
		},
		{
			name:       "too many args",
			args:       []string{"required", "instance", "extra"},
			wantOutput: "command-specific usage\n",
		},
		{
			name:       "parse error preserves command usage",
			args:       []string{"--unknown"},
			wantOutput: "flag provided but not defined: -unknown\ncommand-specific usage\n",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var output strings.Builder
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(&output)
			fs.Usage = func() {
				_, _ = io.WriteString(&output, "command-specific usage\n")
			}

			arg, root, ok := parseRequiredArgOptionalRoot(fs, testCase.args)

			if arg != testCase.wantArg {
				t.Errorf("arg = %q, want %q", arg, testCase.wantArg)
			}
			if root != testCase.wantRoot {
				t.Errorf("root = %q, want %q", root, testCase.wantRoot)
			}
			if ok != testCase.wantOK {
				t.Errorf("ok = %t, want %t", ok, testCase.wantOK)
			}
			if got := output.String(); got != testCase.wantOutput {
				t.Errorf("output = %q, want %q", got, testCase.wantOutput)
			}
		})
	}
}

func TestCommandsExplainFlagsAfterPath(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "status boolean flag",
			args: []string{"status", "missing-instance", "--json"},
			want: `error: flags must precede the path argument (got "--json" after "missing-instance")`,
		},
		{
			name: "runs list alias",
			args: []string{"runs", "list", "missing-instance", "--json"},
			want: `error: flags must precede the path argument (got "--json" after "missing-instance")`,
		},
		{
			name: "validate boolean flag",
			args: []string{"validate", "missing-instance", "--strict"},
			want: `error: flags must precede the path argument (got "--strict" after "missing-instance")`,
		},
		{
			name: "doctor value flag",
			args: []string{"doctor", "--repo", "missing-instance", "--report", "json"},
			want: `error: flags must precede the path argument (got "--report" after "missing-instance")`,
		},
		{
			name: "unknown flag",
			args: []string{"status", "missing-instance", "--unknown"},
			want: `error: flags must precede the path argument (got "--unknown" after "missing-instance")`,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, testCase.args...)
			if code != 2 {
				t.Fatalf("code = %d, want 2", code)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
			if !strings.HasPrefix(stderr, testCase.want+"\nUsage: ") {
				t.Fatalf("stderr = %q, want diagnostic before usage", stderr)
			}
		})
	}
}

func TestRejectFlagAfterPathHonorsExplicitSeparator(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Bool("json", false, "")
	args := []string{"--", "-literal-path", "--literal-suffix"}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	if rejectFlagAfterPath(fs, args, &stderr) {
		t.Fatalf("explicitly positional arguments were rejected: %q", stderr.String())
	}
}

func TestFlagParsingEndedExplicitlyDoesNotMistakeFlagValueForSeparator(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("name", "", "")
	args := []string{"--name", "--", "path", "--json"}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if flagParsingEndedExplicitly(fs, args) {
		t.Fatal("a non-boolean flag value of -- was mistaken for the argument separator")
	}
}
