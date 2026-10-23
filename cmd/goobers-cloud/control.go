package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	cloudauth "github.com/goobers/goobers/internal/cloud/podauth"
	"github.com/goobers/goobers/internal/controlplane/planes"
	"github.com/goobers/goobers/internal/httpapi"
	sharedauth "github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/version"
)

const controlHelp = `Usage: goobers-cloud control [DATABASE OPTIONS] [OPTIONS]

Serve API and the single-active, fenced cloud controller. Never executes stages.
Database options (separate credentials; no role membership):
  --instance-id ID                          Required instance (or GOOBERS_INSTANCE_ID)
  --api-database-url URL                    API transaction-pool URL
  --api-database-url-file FILE              API URL file
  --api-database-direct-url URL             API direct URL
  --api-database-direct-url-file FILE       API direct URL file
  --controller-database-url URL             Controller transaction-pool URL
  --controller-database-url-file FILE       Controller URL file
  --controller-database-direct-url URL      Controller direct URL
  --controller-database-direct-url-file FILE Controller direct URL file
  --database-auth MODE                      password or entra (default password)
  --database-max-conns N                    Combined budget (default 16)
  --database-direct-max-conns N             Direct reservation per role (default 2)

Environment: GOOBERS_API_DATABASE_URL, GOOBERS_API_DATABASE_DIRECT_URL,
GOOBERS_CONTROLLER_DATABASE_URL, GOOBERS_CONTROLLER_DATABASE_DIRECT_URL.
Both roles must target goobers_<instance>, with dashes changed to underscores.

Options:
  --check-assembly              Print compiled plane wiring and exit (offline)
  --listen ADDRESS              API/probes address (default 127.0.0.1:8080)
  --tls-cert FILE                API TLS certificate (required off loopback)
  --tls-key FILE                 API TLS private key
  --pod-key-file FILE            Shared signed pod-token key file (required)
  --self-execution POLICY        Must be deny (default deny)
  --temporal-hostport HOST:PORT  Temporal address (default localhost:7233)
  --temporal-namespace NAME      Temporal namespace (default default)
  --task-queue NAME              Remote engine task queue (default goobers)

Readiness requires schema compatibility, complete plane/recovery wiring, and a
live controller lease. SIGTERM stops controller loops and releases authority.
`

type controlOptions struct {
	listen, cert, key, podKey, selfExecution, host, namespace, queue string
	checkAssembly                                                    bool
}

func (o *controlOptions) flags(f *flag.FlagSet) {
	f.BoolVar(&o.checkAssembly, "check-assembly", false, "check compiled plane wiring without dependencies")
	f.StringVar(&o.listen, "listen", "127.0.0.1:8080", "API/probe address")
	f.StringVar(&o.cert, "tls-cert", "", "TLS certificate")
	f.StringVar(&o.key, "tls-key", "", "TLS private key")
	f.StringVar(&o.podKey, "pod-key-file", environmentOr("GOOBERS_POD_KEY_FILE", ""), "signed pod key file")
	f.StringVar(&o.selfExecution, "self-execution", "deny", "must be deny")
	f.StringVar(&o.host, "temporal-hostport", "localhost:7233", "Temporal address")
	f.StringVar(&o.namespace, "temporal-namespace", "default", "Temporal namespace")
	f.StringVar(&o.queue, "task-queue", "goobers", "engine task queue")
}
func runControlRole(args []string, stdout, stderr io.Writer) int {
	return runControlConfigured(args, stdout, stderr)
}

func validateControlOptions(options controlOptions) error {
	return control.ValidateBoot(options.selfExecution, nil)
}

// controlPlanes is the composition seam for independently landing PG backends.
// No local filesystem implementation or successful no-op recovery is installed.
type controlPlanes struct {
	// EngineChildAdmissions installs the service's HTTP plane once #7879's
	// service and worker client composition land together.
	EngineChildAdmissions httpapi.HandlerOption
	Configuration         control.Configuration
	Reader                readservice.Reader
	Claims                httpapi.ClaimService
	Triggers              httpapi.TriggerService
	Journal               httpapi.JournalService
	RunJournal            httpapi.RunJournalService
	Resolve               acceptance.StartInputResolver
	Epoch                 func(context.Context, *lease.Handle) (control.EpochServices, error)
	RunnerHosts           []string
}

func (p controlPlanes) complete() bool {
	return len(p.missing()) == 0
}

// The assembly receives all auth surfaces, including the verifier required by
// credential refresh and durable revocations. Its backends must fence writes.
type controlAuth struct {
	PodKeys, GrantKeys    *sharedauth.SignedKey
	VerifyCredentialGrant func(context.Context, string) (sharedauth.CredentialGrant, error)
	RevokeGrant           func(context.Context, string, time.Time) error
	Revocations           planes.GrantRevocations
}

var assembleControlPlanes = composeControlPlanes

func serveControl(ctx context.Context, db controlDatabases, o controlOptions, logger *log.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	instanceID := db.InstanceID
	manager, err := lease.New(lease.Config{Pool: db.Controller.Pools.Direct, InstanceID: instanceID, Name: lease.ControllerName, Holder: environmentOr("GOOBERS_REPLICA_ID", defaultMigrationHolder())})
	if err != nil {
		return err
	}
	var activeHost atomic.Pointer[control.Host]
	liveOwner := func() *lease.Handle {
		host := activeHost.Load()
		if host == nil {
			return nil
		}
		return host.Owner()
	}
	revocations := &cloudauth.Revocations{Pool: db.API.Pools.Transaction, InstanceID: instanceID}
	auth, err := cloudauth.New(cloudauth.Config{Mode: "signed", KeyFile: o.podKey, Revocations: revocations, Fallback: httpapi.DenyAllAuthenticator{}})
	if err != nil {
		return err
	}
	services, err := assembleControlPlanes(ctx, db, controlAuth{PodKeys: auth.PodKeys, GrantKeys: auth.GrantKeys, VerifyCredentialGrant: auth.VerifyCredentialGrant, RevokeGrant: revocations.Revoke, Revocations: revocations}, liveOwner)
	if err != nil {
		return err
	}
	if err := control.ValidateBoot(o.selfExecution, services.RunnerHosts); err != nil {
		return err
	}
	if err := services.validate(); err != nil {
		return err
	}
	temporal, err := client.DialContext(ctx, client.Options{HostPort: o.host, Namespace: o.namespace})
	if err != nil {
		return err
	}
	defer temporal.Close()
	dialed := epoch.Temporal(temporal)
	epochTemporal.Store(&dialed)
	report := func(err error) { logger.Printf("%v", err) }
	runtime := control.Runtime{Configuration: services.Configuration, Pool: db.Controller.Pools.Transaction, InstanceID: instanceID, Lease: manager, Temporal: temporal, TaskQueue: o.queue, Resolve: services.Resolve, Services: services.Epoch, Report: report}
	host, err := control.New(control.Config{ValidateAssembly: services.validate, SelfExecution: o.selfExecution, RunnerHosts: services.RunnerHosts, Leadership: manager, SchemaCompatible: func(ctx context.Context) error {
		return checkCloudSchema(ctx, db.Controller.Pools.Transaction, version.Get().Version)
	}, BuildEpoch: runtime.Build, Report: report})
	if err != nil {
		return err
	}
	activeHost.Store(host)
	handler, err := controlHandler(db.API, services, auth, host, logger)
	if err != nil {
		return err
	}
	var opts []httpapi.ServerOption
	if o.cert != "" || o.key != "" {
		opts = append(opts, httpapi.WithTLS(o.cert, o.key))
	}
	server, err := httpapi.NewServer(o.listen, handler, logger, opts...)
	if err != nil {
		return err
	}
	if err := server.Start(); err != nil {
		return err
	}
	logger.Printf("serving %s://%s", server.Scheme(), server.Address())
	return runControlLifetime(ctx, cancel, host, auth, server, report)
}

func controlHandler(db databaseServices, p controlPlanes, auth *cloudauth.Auth, host *control.Host, logger *log.Logger) (http.Handler, error) {
	var handler http.Handler
	var err error
	if p.complete() {
		handler, err = httpapi.NewHandler(p.Reader, httpapi.RequireRoles(), logger, httpapi.WithAuthenticator(auth), httpapi.WithClaimService(p.Claims), httpapi.WithTriggerService(p.Triggers), httpapi.WithJournalService(p.Journal), httpapi.WithRunJournalService(p.RunJournal), httpapi.WithRecoveryGate(host.Ready), p.EngineChildAdmissions)
	}
	if err != nil {
		return nil, err
	}
	handler, err = httpapi.NewControlStatusHandler(auth, func(ctx context.Context) (any, error) {
		rows, err := db.StartStatusPage(ctx, "", 100)
		return struct {
			Ready  bool                     `json:"ready"`
			Starts []acceptance.StartStatus `json:"uncertainStarts"`
		}{host.Ready(), rows}, err
	}, handler)
	if err != nil {
		return nil, err
	}
	return httpapi.WrapWithProbes(handler, func() bool { return true }, func() httpapi.ReadinessStatus {
		return httpapi.ReadinessStatus{Ready: host.Ready() && p.complete(), SchedulerReady: host.Ready()}
	}), nil
}

func runControlLifetime(ctx context.Context, cancel context.CancelFunc, host *control.Host, auth *cloudauth.Auth, server *httpapi.Server, report func(error)) error {
	done := make(chan error, 2)
	go func() { done <- host.Run(ctx) }()
	go func() { done <- auth.RunKeyReload(ctx, report) }()
	var err error
	completed := 0
	select {
	case <-ctx.Done():
	case err = <-done:
		completed++
	case err = <-server.Errors():
	}
	if err == context.Canceled { //nolint:errorlint // Only bare cancellation is benign; joined shutdown failures must survive.
		err = nil
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	err = errors.Join(err, server.Shutdown(shutdown))
	for completed < 2 {
		select {
		case result := <-done:
			completed++
			if result != context.Canceled { //nolint:errorlint // Preserve fatal errors joined with cancellation.
				err = errors.Join(err, result)
			}
		case <-shutdown.Done():
			return errors.Join(err, shutdown.Err())
		}
	}
	return err
}

func (p controlPlanes) validate() error {
	return validateControlAssembly(p)
}

func (o *controlDatabaseOptions) flags(f *flag.FlagSet) {
	f.StringVar(&o.apiURL, "api-database-url", "", "API transaction pool URL")
	f.StringVar(&o.apiFile, "api-database-url-file", "", "API transaction URL file")
	f.StringVar(&o.apiDirect, "api-database-direct-url", "", "API direct pool URL")
	f.StringVar(&o.apiDirectFile, "api-database-direct-url-file", "", "API direct URL file")
	f.StringVar(&o.controllerURL, "controller-database-url", "", "controller transaction pool URL")
	f.StringVar(&o.controllerFile, "controller-database-url-file", "", "controller transaction URL file")
	f.StringVar(&o.controllerDirect, "controller-database-direct-url", "", "controller direct pool URL")
	f.StringVar(&o.controllerDirectFile, "controller-database-direct-url-file", "", "controller direct URL file")
	f.StringVar(&o.instanceID, "instance-id", environmentOr("GOOBERS_INSTANCE_ID", ""), "required non-placeholder instance identity")
	f.StringVar(&o.auth, "database-auth", environmentOr("GOOBERS_DATABASE_AUTH", "password"), "password or entra")
	f.IntVar(&o.max, "database-max-conns", int(cloudpg.DefaultMaxConnections), "total connection budget across both roles")
	f.IntVar(&o.direct, "database-direct-max-conns", int(cloudpg.DefaultDirectMaxConnections), "direct connection reservation per role")
}
