package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/iossimulator"
)

const (
	iosSimulatorResultFile = "ios-simulator-result.json"
	iosSimulatorOutputTail = 64 << 10
)

const iosSimulatorTestHelp = `Usage: goobers ios-simulator-test (--project <path> | --workspace <path>) --scheme <name> [flags]

Run an XCUITest scheme on an available iPhone simulator, parse the xcresult
summary, and write flat workflow outputs to GOOBERS_INPUT_RESULTFILE. A workflow
using this command must declare runner requirements os=darwin and xcode so
scheduling rejects incompatible hosts before invocation. The result records the
selected Xcode, simulator device, and runtime versions; failure output includes
the parsed xcresult test diagnostics.

Flags:
  --project <path>         Xcode project path (mutually exclusive with --workspace)
  --workspace <path>       Xcode workspace path (mutually exclusive with --project)
  --scheme <name>          shared test scheme to run
  --device <name>          exact simulator device name (default: first available iPhone)
  --runtime <version>      iOS runtime version, name, or identifier (default: latest available)
  --only-testing <target>  optional xcodebuild only-testing selector
  --result-bundle <path>   relative xcresult bundle path (default: ios-simulator.xcresult)
`

type iosSimulatorCommandDeps struct {
	hostOS string
	tools  iossimulator.ToolRunner
}

func runIOSSimulatorTest(args []string, stdout, stderr io.Writer) int {
	return runIOSSimulatorTestWith(args, stdout, stderr, iosSimulatorCommandDeps{
		hostOS: runtime.GOOS,
		tools:  iossimulator.ExecToolRunner{},
	})
}

func runIOSSimulatorTestWith(args []string, stdout, stderr io.Writer, deps iosSimulatorCommandDeps) int {
	flags := newCLIFlagSet("ios-simulator-test", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = helpUsage(stderr, "ios-simulator-test")
	var options iossimulator.Options
	flags.StringVar(&options.Project, "project", "", "Xcode project path")
	flags.StringVar(&options.Workspace, "workspace", "", "Xcode workspace path")
	flags.StringVar(&options.Scheme, "scheme", "", "shared test scheme")
	flags.StringVar(&options.Device, "device", "", "simulator device name")
	flags.StringVar(&options.Runtime, "runtime", "", "iOS runtime")
	flags.StringVar(&options.OnlyTesting, "only-testing", "", "xcodebuild only-testing selector")
	flags.StringVar(&options.ResultBundle, "result-bundle", iossimulator.DefaultResultBundle, "relative xcresult bundle path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		pf(stderr, "goobers ios-simulator-test: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		flags.Usage()
		return 2
	}
	if err := iossimulator.ValidateOptions(options); err != nil {
		pf(stderr, "goobers ios-simulator-test: %v\n", err)
		return 2
	}

	resultPath := os.Getenv(executor.InputEnvVar(executor.InputResultFile))
	if resultPath == "" {
		resultPath = iosSimulatorResultFile
	}
	if err := iossimulator.ValidateHost(deps.hostOS); err != nil {
		result := iossimulator.Result{
			Outcome:           "error",
			ResultBundlePath:  filepath.ToSlash(options.ResultBundle),
			DiagnosticSummary: err.Error(),
			ErrorCode:         "unsupported_host",
			ErrorMessage:      err.Error(),
		}
		if writeErr := writeIOSSimulatorResult(resultPath, result); writeErr != nil {
			pf(stderr, "goobers ios-simulator-test: %v; write result: %v\n", err, writeErr)
			return 2
		}
		pf(stderr, "goobers ios-simulator-test: %v\n", err)
		return 1
	}

	report := iossimulator.Run(context.Background(), options, deps.tools)
	if len(report.XcodeOutput) > 0 {
		_, _ = stdout.Write(tailBytes(report.XcodeOutput, iosSimulatorOutputTail))
		if report.XcodeOutput[len(report.XcodeOutput)-1] != '\n' {
			_, _ = io.WriteString(stdout, "\n")
		}
	}
	if len(report.SummaryOutput) > 0 {
		target := stdout
		if !report.Result.Passed {
			target = stderr
		}
		_, _ = fmt.Fprintf(target, "xcresult summary:\n%s\n", report.SummaryOutput)
	}
	if err := writeIOSSimulatorResult(resultPath, report.Result); err != nil {
		pf(stderr, "goobers ios-simulator-test: write result: %v\n", err)
		return 2
	}
	if !report.Result.Passed {
		pf(stderr, "goobers ios-simulator-test: %s\n", report.Result.DiagnosticSummary)
		return 1
	}
	pf(stdout, "iOS simulator tests passed: %s; %s; %s\n",
		report.Result.XcodeVersion, report.Result.SimulatorName, report.Result.SimulatorRuntime)
	return 0
}

func writeIOSSimulatorResult(path string, result iossimulator.Result) error {
	if filepath.IsAbs(path) {
		return fmt.Errorf("result file %q must be relative to the stage workspace", path)
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("result file %q must stay inside the stage workspace", path)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(clean))
	if err != nil {
		return fmt.Errorf("resolve result file parent: %w", err)
	}
	workspace, err := filepath.Abs(".")
	if err != nil {
		return fmt.Errorf("resolve stage workspace: %w", err)
	}
	full, err := filepath.Abs(filepath.Join(parent, filepath.Base(clean)))
	if err != nil {
		return fmt.Errorf("resolve result file: %w", err)
	}
	relative, err := filepath.Rel(workspace, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("result file %q must stay inside the stage workspace", path)
	}
	if info, lstatErr := os.Lstat(full); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("result file %q must not be a symlink", path)
	} else if lstatErr != nil && !os.IsNotExist(lstatErr) {
		return fmt.Errorf("inspect result file: %w", lstatErr)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(full, data, 0o644)
}

func tailBytes(data []byte, limit int) []byte {
	if len(data) <= limit {
		return data
	}
	return data[len(data)-limit:]
}
