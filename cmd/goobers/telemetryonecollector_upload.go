package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/onecollector"
)

const telemetryOneCollectorUploadHelp = "Usage: goobers telemetry onecollector upload --profile-file PATH --input NAME [--validate-only]\n\nValidate one bounded JSON input from stdin using the protected profile. Validation\nnever resolves credentials or sends data. Upload additionally requires the profile's\nenvironment opt-in; it never retries uncertain delivery.\n"
const telemetryOneCollectorMCPHelp = "Usage: goobers telemetry onecollector mcp --profile-file PATH\n\nServe only the protected profile's declared tools over bounded newline-delimited\nMCP stdio. Tool discovery and validation never resolve upload credentials.\n"

type collectorReceipt struct {
	Success     bool   `json:"success"`
	Category    string `json:"category"`
	HTTPStatus  int    `json:"httpStatus,omitempty"`
	RecordCount int    `json:"recordCount,omitempty"`
	Error       string `json:"error,omitempty"`
}

func collectorExplicitUpload(ctx context.Context, profile onecollector.Profile, input string, raw []byte, validateOnly bool) collectorReceipt {
	envelopes, err := profile.PrepareInput(input, raw)
	if err != nil {
		return collectorReceipt{Category: "validation", Error: err.Error()}
	}
	if validateOnly {
		return collectorReceipt{Success: true, Category: "validated", RecordCount: len(envelopes)}
	}
	if !profile.Enabled() {
		return collectorReceipt{Category: "disabled", Error: onecollector.ErrDisabled.Error()}
	}
	registry, scrubber := journal.DefaultScrubber()
	var cfg telemetry.Config
	if err := runtimewiring.ConfigureOneCollectorProfile(ctx, &cfg, profile, registry, nil); err != nil {
		return collectorReceipt{Category: "internal", Error: "onecollector: credential resolution failed"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, err := telemetry.UploadOneCollector(ctx, *cfg.OneCollector, envelopes, scrubber)
	if err != nil {
		var delivery *onecollector.DeliveryError
		if errors.As(err, &delivery) {
			return collectorReceipt{Category: delivery.Category, HTTPStatus: delivery.Status, Error: delivery.Error()}
		}
		return collectorReceipt{Category: "internal", Error: "onecollector: upload failed"}
	}
	return collectorReceipt{Success: true, Category: "uploaded", RecordCount: len(envelopes), HTTPStatus: status}
}

func runOneCollectorUpload(args []string, stdout, stderr io.Writer) int {
	return runOneCollectorUploadWithInput(args, os.Stdin, stdout, stderr)
}

func runOneCollectorUploadWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("telemetry onecollector upload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("profile-file", "", "protected JSON profile")
	input := fs.String("input", "", "profile input name")
	validate := fs.Bool("validate-only", false, "validate without credentials or network")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *path == "" || *input == "" {
		pf(stderr, "%s", telemetryOneCollectorUploadHelp)
		return 2
	}
	profile, err := onecollector.LoadProfile(*path)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, onecollector.MaxInputBytes+1))
	if err != nil {
		pf(stderr, "error: cannot read input\n")
		return 2
	}
	result := collectorExplicitUpload(context.Background(), *profile, *input, raw, *validate)
	if err := writeJSONLine(stdout, result); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if result.Success || result.Category == "disabled" {
		return 0
	}
	return 1
}

func runOneCollectorMCP(args []string, stdout, stderr io.Writer) int {
	return runOneCollectorMCPWithInput(args, os.Stdin, stdout, stderr)
}

func runOneCollectorMCPWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("telemetry onecollector mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("profile-file", "", "protected JSON profile")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *path == "" {
		pf(stderr, "%s", telemetryOneCollectorMCPHelp)
		return 2
	}
	profile, err := onecollector.LoadProfile(*path)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	tools := make([]mcpio.DynamicTool, 0, len(profile.Tools))
	for _, tool := range profile.Tools {
		tools = append(tools, mcpio.DynamicTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	server := mcpio.NewDynamicServer("goobers-onecollector", tools, func(name string, raw json.RawMessage) (any, error) {
		input, payload, validate, err := profile.ToolPayload(name, raw)
		var result collectorReceipt
		if err != nil {
			result = collectorReceipt{Category: "validation", Error: err.Error()}
		} else {
			result = collectorExplicitUpload(context.Background(), *profile, input, payload, validate)
		}
		return mcpio.DynamicResult{Value: result, IsError: !result.Success}, nil
	})
	if err := server.Serve(stdin, stdout, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}
