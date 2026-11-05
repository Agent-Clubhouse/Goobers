package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/cloud/config/readiness"
	"github.com/goobers/goobers/internal/cloud/cutover"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/version"
)

const cutoverHelp = `Usage: goobers-cloud cutover check --config-dir DIR --instance-config FILE [OPTIONS]

Check that a target install is ready to take over from a local instance.

check runs the cloud-readiness assessment over the instance config (every
workflow must be DSL 3.0+, engine-supported and placeable without the control
plane) and the install checks: schema compatible, lease row present,
admissions paused, object-store bucket reachable. It exits non-zero while
anything blocks; a check whose backend has not shipped reports
"not available until #N" and blocks.

Options:
  --config-dir DIR               Instance config directory (required)
  --instance-config FILE         instance.yaml (required)
  --instance-id ID               Instance identifier (required)
  --lease-name NAME              Controller lease name (default controller)
  --format FORMAT                text or json (default text)
  --database-direct-url URL      Direct URL (prefer env/file for credentials)
  --database-direct-url-file PATH  Read direct URL from a mounted Secret
  --database-auth MODE           password or entra (default password)

Environment:
  GOOBERS_DATABASE_DIRECT_URL  Direct PostgreSQL URL
`

func runCutoverRole(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		write(stdout, cutoverHelp)
		return 0
	}
	if len(args) == 0 || args[0] != "check" {
		write(stderr, cutoverHelp)
		return 2
	}
	flags := flag.NewFlagSet("cutover check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configDir := flags.String("config-dir", "", "instance config directory")
	instanceConfig := flags.String("instance-config", "", "instance.yaml path")
	instanceID := flags.String("instance-id", "", "instance identifier")
	leaseName := flags.String("lease-name", cutover.ControllerLeaseName, "controller lease name")
	format := flags.String("format", "text", "text or json")
	directURLArg := flags.String("database-direct-url", "", "direct PostgreSQL URL")
	directURLFile := flags.String("database-direct-url-file", "", "file containing the direct PostgreSQL URL")
	authMode := flags.String("database-auth", environmentOr("GOOBERS_DATABASE_AUTH", "password"), "database authentication mode: password or entra")
	flags.Usage = func() { write(stderr, cutoverHelp) }
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *configDir == "" || *instanceConfig == "" || *instanceID == "" || (*format != "text" && *format != "json") {
		flags.Usage()
		return 2
	}
	cfg, err := instance.LoadConfig(*instanceConfig)
	if err != nil {
		writef(stderr, "goobers-cloud cutover check: %v\n", err)
		return 2
	}
	set, loadReport, err := instance.LoadConfigDir(*configDir)
	if err != nil {
		writef(stderr, "goobers-cloud cutover check: load config: %v\n", err)
		return 2
	}
	// A config with validation errors would silently drop workflows from the
	// assessment, so it can never read as cloud-ready.
	if loadReport != nil && loadReport.HasErrors() {
		for _, issue := range loadReport.Issues {
			if issue.Severity == validate.Error {
				writef(stderr, "goobers-cloud cutover check: %s: %s\n", issue.File, issue.Message)
			}
		}
		return 2
	}
	ctx, stop := signalContext()
	defer stop()
	var install cutover.Install
	if directURL, err := cloudpg.DatabaseURL(*directURLArg, *directURLFile, "GOOBERS_DATABASE_DIRECT_URL"); err == nil {
		config := cloudpg.Config{TransactionURL: directURL, DirectURL: directURL}
		if code := configureDatabaseAuth("cutover check", *authMode, &config, stderr); code != 0 {
			return code
		}
		pools, err := newCloudPools(ctx, config)
		if err != nil {
			writef(stderr, "goobers-cloud cutover check: %s\n", cloudpg.SafeDiagnostic(err))
			return 1
		}
		defer pools.Close()
		install = cutover.PGInstall{Pool: pools.Direct, Binary: version.Get().Version, InstanceID: *instanceID, LeaseName: *leaseName}
	}
	report := runCutoverCheck(ctx, cfg, set, install)
	if *format == "json" {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			writef(stderr, "goobers-cloud cutover check: %v\n", err)
			return 1
		}
	} else if err := report.WriteText(stdout); err != nil {
		writef(stderr, "goobers-cloud cutover check: %v\n", err)
		return 1
	}
	if !report.Ready {
		return 1
	}
	return 0
}

var runCutoverCheck = func(ctx context.Context, cfg *instance.Config, set *instance.ConfigSet, install cutover.Install) cutover.Report {
	return cutover.Run(ctx, readiness.Assess, cfg, set, install)
}
