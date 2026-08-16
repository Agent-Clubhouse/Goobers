// Command scheduler is the Goobers system scheduler process. It loads the
// config-as-code repo, registers workflow definitions, and runs a per-gaggle
// trigger→readiness→dispatch loop that starts workflow runs on Temporal via the
// engine start API. The goober-runtime worker executes those runs.
//
// Tier-3 (V2) — quarantined, not on the V0 path (V0 scheduling is the embedded
// cron evaluator in `goobers up`, ARCHITECTURE.md §7). See docs/ARCHITECTURE.md
// §11. Revived in V2.
//
// Configuration is YAML-first with environment overrides (config-as-code; no UI):
//
//	GOOBERS_CONFIG_DIR         path to the config repo (required)
//	GOOBERS_INSTANCE_ROOT      instance root containing instance.yaml (optional)
//	GOOBERS_TEMPORAL_HOSTPORT  overrides engine.hostPort
//	GOOBERS_TEMPORAL_NAMESPACE overrides engine.namespace
//	GOOBERS_TASK_QUEUE         overrides engine.taskQueue
//	GOOBERS_BACKLOG_TOKEN      backlog provider token (Key Vault-injected)
//	GOOBERS_ADO_AUTH_KIND      pat|azure-cli|workload-identity|managed-identity
//	GOOBERS_POLL_INTERVAL      backlog poll cadence (default 30s)
//	GOOBERS_OTEL_EXPORTER      telemetry exporter: stdout|otlp (default stdout)
//	GOOBERS_OTLP_ENDPOINT      OTLP endpoint when exporter=otlp
//	GOOBERS_ENV                environment label (dev|staging|prod)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/app"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/labelpredicate"
	"github.com/goobers/goobers/internal/scheduler"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/providers"
)

func main() {
	secretReg, scrubber := journal.DefaultScrubber()
	app.MainWithScrubber("scheduler", scrubber, func(ctx context.Context, log *slog.Logger) error {
		return runWithScrubber(ctx, log, secretReg, scrubber)
	})
}

type config struct {
	configDir         string
	namespace         string
	temporalHostPort  string
	temporalNamespace string
	taskQueue         string
	backlogToken      string
	adoAuthKind       string
	adoTenant         string
	adoClientID       string
	pollInterval      time.Duration
	triggerBackoff    time.Duration
	pollLimit         int
	exporter          telemetry.ExporterKind
	otlpEndpoint      string
	environment       string
}

func configFromEnv() (config, error) {
	engineConfig, err := schedulerEngineConfig(os.Getenv("GOOBERS_INSTANCE_ROOT"))
	if err != nil {
		return config{}, err
	}
	return config{
		configDir:         os.Getenv("GOOBERS_CONFIG_DIR"),
		namespace:         os.Getenv("GOOBERS_NAMESPACE"),
		temporalHostPort:  engineConfig.HostPort,
		temporalNamespace: engineConfig.Namespace,
		taskQueue:         engineConfig.TaskQueue,
		backlogToken:      os.Getenv("GOOBERS_BACKLOG_TOKEN"),
		adoAuthKind:       os.Getenv("GOOBERS_ADO_AUTH_KIND"),
		adoTenant:         os.Getenv("GOOBERS_ADO_TENANT"),
		adoClientID:       envOr("GOOBERS_ADO_CLIENT_ID", os.Getenv("AZURE_CLIENT_ID")),
		pollInterval:      envDuration("GOOBERS_POLL_INTERVAL", 30*time.Second),
		triggerBackoff:    envDuration("GOOBERS_TRIGGER_BACKOFF", 5*time.Second),
		pollLimit:         100,
		exporter:          telemetry.ExporterKind(envOr("GOOBERS_OTEL_EXPORTER", string(telemetry.ExporterStdout))),
		otlpEndpoint:      os.Getenv("GOOBERS_OTLP_ENDPOINT"),
		environment:       os.Getenv("GOOBERS_ENV"),
	}, nil
}

func schedulerEngineConfig(root string) (instance.EngineConfig, error) {
	if root != "" {
		cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
		if err != nil {
			return instance.EngineConfig{}, err
		}
		return cfg.EffectiveEngineConfig(), nil
	}
	resolved, _, err := (&instance.Config{}).ResolveEngineConfig(os.LookupEnv)
	return resolved, err
}

func runWithScrubber(ctx context.Context, log *slog.Logger, secretReg *journal.RegistryScrubber, scrubber journal.Scrubber) error {
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	if cfg.configDir == "" {
		return errors.New("GOOBERS_CONFIG_DIR is required")
	}

	loaded, err := bootstrap.LoadAndRegister(cfg.configDir, cfg.namespace)
	if err != nil {
		return err
	}

	tel, err := telemetry.New(ctx, schedulerTelemetryConfig(cfg, scrubber))
	if err != nil {
		return err
	}
	defer func() { _ = tel.Shutdown(context.Background()) }()

	tc, err := bootstrap.DialTemporal(cfg.temporalHostPort, cfg.temporalNamespace)
	if err != nil {
		return err
	}
	defer tc.Close()
	starter := bootstrap.NewStarter(tc, cfg.taskQueue)

	var wg sync.WaitGroup
	for _, g := range loaded.Gaggles {
		adoSource, sourceErr := schedulerADOCredentialSource(g.Spec.Backlog.Provider, cfg)
		if sourceErr != nil {
			log.Warn("skipping gaggle: ADO credential source", "gaggle", g.Name, "err", sourceErr)
			continue
		}
		provider, repo, perr := bootstrap.BacklogProviderFor(g.Spec.Backlog, cfg.backlogToken, adoSource, secretReg, tel)
		if perr != nil {
			log.Warn("skipping gaggle: backlog provider", "gaggle", g.Name, "err", perr)
			continue
		}
		workflows := loaded.BacklogWorkflows(g.Name)
		if len(workflows) == 0 {
			log.Info("gaggle has no backlog-triggered workflows", "gaggle", g.Name)
			continue
		}
		sched, serr := loaded.SchedulerFor(g.Name, bootstrap.SchedulerDeps{
			Starter:   starter,
			Telemetry: tel,
			Claimer:   scheduler.BacklogClaimer{Provider: provider, Repo: repo},
		})
		if serr != nil {
			return serr
		}

		events := make(chan scheduler.Event)
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if err := sched.Serve(ctx, events, decisionLogger(log, name)); err != nil && ctx.Err() == nil {
				log.Error("scheduler serve loop exited unexpectedly", "gaggle", name, "err", err)
			}
		}(g.Name)

		predicate, predicateErr := labelpredicate.Compile(g.Spec.Backlog.LabelPredicate, g.Spec.Backlog.Labels, nil)
		if predicateErr != nil {
			return fmt.Errorf("gaggle %q backlog label predicate: %w", g.Name, predicateErr)
		}
		for _, wfName := range workflows {
			fieldPredicate, fieldPredicateErr := backlogFieldPredicateForWorkflow(g, loaded.Workflows, wfName)
			if fieldPredicateErr != nil {
				return fieldPredicateErr
			}
			tk := time.NewTicker(cfg.pollInterval)
			tr := scheduler.BacklogPollTrigger{
				WorkflowName:   wfName,
				Provider:       provider,
				Repo:           repo,
				Labels:         predicate.RequiredLabels(),
				LabelPredicate: predicate,
				FieldPredicate: fieldPredicate,
				Ticks:          tk.C,
				Limit:          cfg.pollLimit,
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer tk.Stop()
				superviseTrigger(ctx, log, tr, events, cfg.triggerBackoff)
			}()
		}
		log.Info("scheduler online for gaggle", "gaggle", g.Name, "workflows", workflows, "pollInterval", cfg.pollInterval.String())
	}

	<-ctx.Done()
	wg.Wait()
	return nil
}

func backlogFieldPredicateForWorkflow(gaggle apiv1.Gaggle, workflows []apiv1.Workflow, workflowName string) (*fieldpredicate.Predicate, error) {
	for i := range workflows {
		workflow := &workflows[i]
		if workflow.Name != workflowName || workflow.Spec.Gaggle != gaggle.Name {
			continue
		}
		for _, trigger := range workflow.Spec.Triggers {
			if trigger.Type != apiv1.TriggerBacklogItem {
				continue
			}
			predicate, err := fieldpredicate.CompileConjunction(
				gaggle.Spec.Backlog.FieldPredicate,
				trigger.FieldPredicate,
			)
			if err != nil {
				return nil, fmt.Errorf("workflow %q backlog field predicate: %w", workflowName, err)
			}
			return predicate, nil
		}
		break
	}
	return nil, fmt.Errorf("workflow %q in gaggle %q has no backlog-item trigger", workflowName, gaggle.Name)
}

func schedulerADOCredentialSource(provider apiv1.Provider, cfg config) (providers.ADOCredentialSource, error) {
	if provider != apiv1.ProviderADO {
		return nil, nil
	}
	switch cfg.adoAuthKind {
	case "":
		if cfg.backlogToken == "" {
			return nil, fmt.Errorf("ADO backlog requires GOOBERS_ADO_AUTH_KIND or GOOBERS_BACKLOG_TOKEN")
		}
		return nil, nil
	case "pat":
		if cfg.backlogToken == "" {
			return nil, fmt.Errorf("ADO PAT auth requires GOOBERS_BACKLOG_TOKEN")
		}
		return nil, nil
	case "azure-cli":
		return providers.NewAzureCLIADOCredentialSource(nil, cfg.adoTenant), nil
	case "workload-identity":
		return providers.NewWorkloadIdentityADOCredentialSource()
	case "managed-identity":
		return providers.NewManagedIdentityADOCredentialSource(cfg.adoClientID)
	default:
		return nil, fmt.Errorf("unsupported GOOBERS_ADO_AUTH_KIND %q", cfg.adoAuthKind)
	}
}

func schedulerTelemetryConfig(cfg config, scrubber journal.Scrubber) telemetry.Config {
	return telemetry.Config{
		ServiceName:    "scheduler",
		ServiceVersion: version.Get().Version,
		BuildCommit:    version.Get().Commit,
		Environment:    cfg.environment,
		Exporter:       cfg.exporter,
		OTLPEndpoint:   cfg.otlpEndpoint,
		OTLPInsecure:   cfg.otlpEndpoint != "",
		Batch:          true,
		Scrubber:       scrubber,
	}
}

// superviseTrigger runs a trigger, restarting Watch with backoff when it exits
// with an error. BacklogPollTrigger.Watch returns on a provider/list error, so
// without supervision a transient backlog API blip would permanently — and
// silently — stop polling for that workflow while the process kept running. Each
// failure is logged; the loop ends only when the context is cancelled or Watch
// returns cleanly (its source closed).
func superviseTrigger(ctx context.Context, log *slog.Logger, tr scheduler.Trigger, out chan<- scheduler.Event, backoff time.Duration) {
	for {
		err := tr.Watch(ctx, out)
		if err == nil || ctx.Err() != nil {
			return
		}
		log.Error("trigger watch failed; retrying", "trigger", tr.Name(), "err", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

func decisionLogger(log *slog.Logger, gaggle string) scheduler.DecisionHandler {
	return func(ev scheduler.Event, d scheduler.Decision, err error) {
		switch {
		case err != nil:
			log.Error("dispatch failed", "gaggle", gaggle, "workflow", ev.WorkflowName, "err", err)
		case d.Started:
			log.Info("run started", "gaggle", gaggle, "workflow", ev.WorkflowName, "runId", d.RunID)
		default:
			log.Debug("run not started", "gaggle", gaggle, "workflow", ev.WorkflowName, "reason", d.Reason)
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
