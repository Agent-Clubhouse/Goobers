package main

import (
	"context"
	"flag"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/telemetry"
)

const telemetryConfigureHelp = "Usage: goobers telemetry configure (--connection-string-env NAME | --connection-string-file PATH | --connection-string-store STORE/SECRET | --disable) [--profile health|journal|standard|diagnostic] [--json] [path]\n\n" +
	"Configure the customer-owned Application Insights destination in one atomic\n" +
	"instance.yaml update. Only the reference is stored; this command never accepts\n" +
	"or writes an inline connection-string value. The profile defaults to standard,\n" +
	"and bounded 72-hour replay defaults on. Stop a live daemon before changing its\n" +
	"configuration. --disable removes only the direct Azure destination.\n\n" +
	"Exit codes: 0 = configured, 1 = refused/invalid config, 2 = usage or I/O error.\n"

const telemetryTestHelp = "Usage: goobers telemetry test [--destination NAME] [--json] [--timeout DURATION] [path]\n\n" +
	"Resolve the configured connection-string reference and send one fixed,\n" +
	"identity-free connectivity record directly to Application Insights. The probe\n" +
	"bypasses disk replay: success means Azure acknowledged this request now. No\n" +
	"connection-string value is printed, logged, journaled, or persisted.\n\n" +
	"Exit codes: 0 = accepted, 1 = resolution/connection/rejection failure,\n" +
	"2 = usage or invalid configuration.\n"

type telemetryConfigureResult struct {
	Schema    string                              `json:"schema"`
	Enabled   bool                                `json:"enabled"`
	Profile   instance.TelemetryCollectionProfile `json:"profile,omitempty"`
	Source    string                              `json:"source,omitempty"`
	Reference string                              `json:"reference,omitempty"`
}

var testAzureMonitorConnectivity = telemetry.TestAzureMonitorConnectivity

func runTelemetryConfigure(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("telemetry configure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	envRef := fs.String("connection-string-env", "", "environment variable containing the Application Insights connection string")
	fileRef := fs.String("connection-string-file", "", "protected file containing the Application Insights connection string")
	storeRef := fs.String("connection-string-store", "", "declared secret-store reference in STORE/SECRET form")
	profileValue := fs.String("profile", string(instance.TelemetryProfileStandard), "collection profile: health, journal, standard, or diagnostic")
	disable := fs.Bool("disable", false, "remove the direct Azure Monitor destination")
	asJSON := fs.Bool("json", false, "render the result as JSON")
	fs.Usage = helpUsage(stderr, "telemetry configure")
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

	references := 0
	for _, value := range []string{*envRef, *fileRef, *storeRef} {
		if value != "" {
			references++
		}
		if looksLikeApplicationInsightsSecret(value) {
			pf(stderr, "error: connection-string flags accept a reference name or path, never the connection-string value\n")
			return 2
		}
	}
	if (*disable && references != 0) || (!*disable && references != 1) {
		pf(stderr, "error: choose exactly one connection-string reference, or --disable\n")
		return 2
	}

	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		pf(stderr, "error: load config: %v\n", err)
		return 2
	}
	if len(cfg.Telemetry.Exporters) > 0 {
		pf(stderr, "error: telemetry configure manages the legacy azureMonitor block; edit telemetry.exporters in instance.yaml to change named destinations\n")
		return 2
	}
	result := telemetryConfigureResult{Schema: "goobers.dev/telemetry/configure/v1"}
	if *disable {
		cfg.Telemetry.AzureMonitor = nil
	} else {
		enabled := true
		cfg.Telemetry.Enabled = &enabled
		cfg.Telemetry.CollectionProfile = instance.TelemetryCollectionProfile(*profileValue)
		ref := instance.TokenRef{Env: *envRef, File: *fileRef, Store: *storeRef}
		cfg.Telemetry.AzureMonitor = &instance.AzureMonitorConfig{ConnectionString: ref}
		result.Enabled, result.Profile = true, cfg.Telemetry.CollectionProfile
		switch {
		case *envRef != "":
			result.Source, result.Reference = "env", *envRef
		case *fileRef != "":
			result.Source, result.Reference = "file", *fileRef
		case *storeRef != "":
			result.Source, result.Reference = "store", *storeRef
		}
	}
	if err := cfg.Validate(); err != nil {
		pf(stderr, "error: telemetry configuration is invalid: %v\n", err)
		return 1
	}
	if err := prepareManualRoot(layout, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if *asJSON {
		if err := writeJSONLine(stdout, result); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		return 0
	}
	if !result.Enabled {
		pf(stdout, "direct Azure Monitor telemetry disabled\n")
		return 0
	}
	pf(stdout, "direct Azure Monitor telemetry configured: profile=%s source=%s reference=%s replay=72h/536870912-bytes\n", result.Profile, result.Source, result.Reference)
	return 0
}

func looksLikeApplicationInsightsSecret(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "instrumentationkey=") || strings.Contains(lower, "ingestionendpoint=")
}

func runTelemetryTest(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("telemetry test", flag.ContinueOnError)
	destination := fs.String("destination", "", "named Azure destination to probe")
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "render the connectivity result as JSON")
	timeout := fs.Duration("timeout", 10*time.Second, "connectivity deadline (1s through 1m)")
	fs.Usage = helpUsage(stderr, "telemetry test")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 || *timeout < time.Second || *timeout > time.Minute {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		pf(stderr, "telemetry test: load config: %v\n", err)
		return 2
	}
	connection, err := telemetryTestConnection(cfg.Telemetry, *destination)
	if err != nil {
		pf(stderr, "telemetry test: %v\n", err)
		return 2
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		pf(stderr, "telemetry test: configure secret stores: %v\n", err)
		return 1
	}
	ref := connection.CredentialTokenRef("telemetry.azureMonitor.connectionString")
	resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{ref}, stores)
	if err != nil {
		pf(stderr, "telemetry test: configure connection-string resolver: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	connectionString, err := resolver.Resolve(ctx, ref.Name)
	if err != nil {
		pf(stderr, "telemetry test: resolve configured connection-string reference: %v\n", err)
		return 1
	}
	result, err := testAzureMonitorConnectivity(ctx, connectionString, nil)
	if err != nil {
		pf(stderr, "telemetry test: Application Insights did not accept the probe: %v\n", err)
		return 1
	}
	if *asJSON {
		if err := writeJSONLine(stdout, result); err != nil {
			pf(stderr, "telemetry test: %v\n", err)
			return 2
		}
		return 0
	}
	pf(stdout, "Application Insights accepted the connectivity probe: recordId=%s observedAt=%s\n", result.RecordID, result.ObservedAt.Format(time.RFC3339Nano))
	return 0
}

// Keep the injected function's production signature explicit at compile time.
var _ func(context.Context, string, *http.Client) (telemetry.AzureMonitorConnectivityResult, error) = testAzureMonitorConnectivity
