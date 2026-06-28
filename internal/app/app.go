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

// run holds the testable core of Main: it takes its args and log sink as
// parameters and returns an exit code instead of calling os.Exit.
func run(name string, args []string, logOut io.Writer, fn RunFunc) int {
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
