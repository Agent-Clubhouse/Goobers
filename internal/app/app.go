// Package app is the shared entrypoint scaffolding for Goobers control-plane
// binaries. It standardizes flag parsing (--version), structured logging, and
// signal-aware lifecycle so each cmd/<binary>/main.go stays a few lines.
package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/version"
)

// RunFunc is the body of a control-plane binary. It receives a context that is
// cancelled on SIGINT/SIGTERM and a configured structured logger. Returning a
// non-nil error causes the process to log it and exit non-zero.
type RunFunc func(ctx context.Context, log *slog.Logger) error

// Scrubber removes secrets from process output before it reaches the log sink.
type Scrubber interface {
	Scrub([]byte) []byte
}

// Main is the canonical entrypoint wrapper. Call it from a binary's main():
//
//	func main() {
//	    app.Main("scheduler", func(ctx context.Context, log *slog.Logger) error {
//	        // ... start serving until ctx is done ...
//	        <-ctx.Done()
//	        return nil
//	    })
//	}
//
// It parses common flags, builds the logger, installs the signal context, runs
// fn, and translates the outcome into a process exit code. It never returns.
func Main(name string, fn RunFunc) {
	os.Exit(run(name, os.Args[1:], os.Stderr, fn))
}

// MainWithScrubber is Main with redaction applied to all process output.
func MainWithScrubber(name string, scrubber Scrubber, fn RunFunc) {
	os.Exit(runWithScrubber(name, os.Args[1:], os.Stderr, scrubber, fn))
}

// run holds the testable core of Main: it takes its args and log sink as
// parameters and returns an exit code instead of calling os.Exit.
func run(name string, args []string, logOut io.Writer, fn RunFunc) int {
	return runWithScrubber(name, args, logOut, nil, fn)
}

func runWithScrubber(name string, args []string, logOut io.Writer, scrubber Scrubber, fn RunFunc) int {
	if scrubber != nil {
		logOut = scrubbedWriter{dst: logOut, scrubber: scrubber}
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(logOut)
	var (
		showVersion = fs.Bool("version", false, "print version information and exit")
		logLevel    = fs.String("log-level", "info", "log level: debug, info, warn, error")
		logFormat   = fs.String("log-format", "json", "log format: json or text")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *showVersion {
		_, _ = fmt.Fprintf(logOut, "%s %s\n", name, version.Get())
		return 0
	}

	log := newLogger(logOut, *logLevel, *logFormat).With("component", name)
	log.Info("starting", "version", version.Get().String())

	ctx, stop := signals.SetupSignalContext()
	defer stop()

	if err := fn(ctx, log); err != nil {
		log.Error("exiting on error", "err", err)
		return 1
	}
	log.Info("shutdown complete")
	return 0
}

type scrubbedWriter struct {
	dst      io.Writer
	scrubber Scrubber
}

func (w scrubbedWriter) Write(p []byte) (int, error) {
	scrubbed := w.scrubber.Scrub(p)
	n, err := w.dst.Write(scrubbed)
	if err != nil {
		return 0, err
	}
	if n != len(scrubbed) {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

// newLogger builds a slog.Logger for the requested level and format. Unknown
// values fall back to info/json so a typo never silently disables logging.
func newLogger(out io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(out, opts)
	} else {
		h = slog.NewJSONHandler(out, opts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
