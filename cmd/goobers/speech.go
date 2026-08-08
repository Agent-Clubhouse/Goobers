package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/speechnotify"
)

const speechTestPhrase = "Goobers speech notifications are ready."

const speechHelp = "Usage: goobers speech <command> [flags] [path]\n\n" +
	"Check and test the configured local speech notification sink without cloud\n" +
	"credentials. Both commands work while speech.enabled is false so audio can\n" +
	"be verified before monitoring is enabled.\n\n" +
	"Commands:\n" +
	"  preflight  report the selected engine and local prerequisites without sound\n" +
	"  test       speak the fixed Goobers readiness phrase\n"

const speechPreflightHelp = "Usage: goobers speech preflight [--json] [path]\n\n" +
	"Report the selected engine and executable, effective voice, language, and\n" +
	"rate, plus the required local audio session. This command emits no sound.\n" +
	"Exit codes: 0 = available, 1 = unavailable or invalid config, 2 = usage error.\n"

const speechTestHelp = "Usage: goobers speech test [--json] [path]\n\n" +
	"Run preflight, then speak the fixed phrase \"" + speechTestPhrase + "\" using\n" +
	"the configured local engine. Arbitrary text is not accepted. The delivery\n" +
	"receipt is appended under scheduler/" + speechnotify.ReceiptFileName + ".\n" +
	"Exit codes: 0 = delivered, 1 = unavailable or failed, 2 = usage error.\n"

var newNativeSpeechSink = speechnotify.NewNative

func runSpeech(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		pf(stdout, "%s", speechHelp)
		return 0
	}
	if len(args) > 0 {
		pf(stderr, "error: unknown speech command %q\n", args[0])
	}
	pf(stderr, "%s", speechHelp)
	return 2
}

func runSpeechPreflight(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("speech preflight", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "render the preflight report as JSON")
	fs.Usage = helpUsage(stderr, "speech preflight")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	sink, config, err := loadSpeechSink(root, nil)
	if err != nil {
		pf(stderr, "speech preflight: %v\n", err)
		return 1
	}
	report, err := preflightSpeech(context.Background(), sink, config)
	if renderErr := writeSpeechPreflight(stdout, report, *asJSON); renderErr != nil {
		pf(stderr, "speech preflight: %v\n", renderErr)
		return 1
	}
	if err != nil {
		pf(stderr, "speech preflight: %v\n", err)
		return 1
	}
	return 0
}

func runSpeechTest(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("speech test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "render the final delivery receipt as JSON")
	fs.Usage = helpUsage(stderr, "speech test")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	recorder := speechnotify.NewFileRecorder(filepath.Join(instance.NewLayout(root).SchedulerDir(), speechnotify.ReceiptFileName))
	sink, config, err := loadSpeechSink(root, recorder)
	if err != nil {
		pf(stderr, "speech test: %v\n", err)
		return 1
	}
	if _, err := preflightSpeech(context.Background(), sink, config); err != nil {
		pf(stderr, "speech test preflight: %v\n", err)
		return 1
	}
	timeout, _ := config.TimeoutDuration()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	receipt, err := sink.Deliver(ctx, speechnotify.Request{
		NotificationID: "speech-test",
		Text:           speechTestPhrase,
	})
	if *asJSON {
		if renderErr := writeJSONLine(stdout, receipt); renderErr != nil {
			pf(stderr, "speech test: %v\n", renderErr)
			return 1
		}
	} else {
		pf(stdout, "speech test %s via %s in %dms\n", receipt.Status, receipt.Engine, receipt.DurationMillis)
	}
	if err != nil {
		pf(stderr, "speech test: %v\n", err)
		return 1
	}
	return 0
}

func loadSpeechSink(root string, recorder speechnotify.Recorder) (*speechnotify.Sink, speechnotify.Config, error) {
	config, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		return nil, speechnotify.Config{}, fmt.Errorf("load config: %w", err)
	}
	speechConfig := config.EffectiveSpeechConfig()
	sink, err := newNativeSpeechSink(speechConfig, recorder)
	if err != nil {
		return nil, speechConfig, err
	}
	return sink, speechConfig, nil
}

func preflightSpeech(ctx context.Context, sink *speechnotify.Sink, config speechnotify.Config) (speechnotify.Preflight, error) {
	timeout, _ := config.TimeoutDuration()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return sink.Preflight(ctx)
}

func writeSpeechPreflight(w io.Writer, report speechnotify.Preflight, asJSON bool) error {
	if asJSON {
		return writeJSONLine(w, report)
	}
	pf(w, "engine: %s\n", report.Engine)
	pf(w, "executable: %s\n", report.Executable)
	pf(w, "voice: %s\n", report.Voice)
	pf(w, "language: %s\n", report.Language)
	pf(w, "rate: %d words/minute\n", report.Rate)
	pf(w, "audio prerequisite: %s\n", report.AudioPrerequisite)
	pf(w, "audio available: %t\n", report.AudioAvailable)
	return nil
}

func writeJSONLine(w io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	if _, err := fmt.Fprintf(w, "%s\n", encoded); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}
