// Command scheduler is the Goobers system scheduler process. It loads the
// config-as-code repo, registers workflow definitions, and runs a per-gaggle
// trigger→readiness→dispatch loop that starts workflow runs on Temporal via the
// engine start API. The goober-runtime worker executes those runs.
//
// Tier-3 (V2) — quarantined, not on the V0 path (V0 scheduling is the embedded
// cron evaluator in `goobers up`, ARCHITECTURE.md §7). See docs/ARCHITECTURE.md
// §11. Revived in V2.
//
// Configuration is via environment (config-as-code; no UI):
//
//	GOOBERS_CONFIG_DIR         path to the config repo (required)
//	GOOBERS_TEMPORAL_HOSTPORT  Temporal frontend (default 127.0.0.1:7233)
//	GOOBERS_TEMPORAL_NAMESPACE Temporal namespace (default "default")
//	GOOBERS_TASK_QUEUE         engine task queue (default goobers-engine)
//	GOOBERS_BACKLOG_TOKEN      backlog provider token (Key Vault-injected)
//	GOOBERS_POLL_INTERVAL      backlog poll cadence (default 30s)
//	GOOBERS_OTEL_EXPORTER      telemetry exporter: stdout|otlp (default stdout)
//	GOOBERS_OTLP_ENDPOINT      OTLP endpoint when exporter=otlp
//	GOOBERS_ENV                environment label (dev|staging|prod)
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/app"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/scheduler"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/version"
)

func main() {
	app.Main("scheduler", run)
}

type config struct {
	configDir         string
	namespace         string
	temporalHostPort  string
	temporalNamespace string
	taskQueue         string
	backlogToken      string
	pollInterval      time.Duration
	triggerBackoff    time.Duration
	pollLimit         int
	exporter          telemetry.ExporterKind
	otlpEndpoint      string
	environment       string
}

func configFromEnv() config {
	return config{
		configDir:         os.Getenv("GOOBERS_CONFIG_DIR"),
		namespace:         os.Getenv("GOOBERS_NAMESPACE"),
		temporalHostPort:  envOr("GOOBERS_TEMPORAL_HOSTPORT", "127.0.0.1:7233"),
		temporalNamespace: envOr("GOOBERS_TEMPORAL_NAMESPACE", "default"),
		taskQueue:         envOr("GOOBERS_TASK_QUEUE", bootstrap.DefaultTaskQueue),
		backlogToken:      os.Getenv("GOOBERS_BACKLOG_TOKEN"),
		pollInterval:      envDuration("GOOBERS_POLL_INTERVAL", 30*time.Second),
		triggerBackoff:    envDuration("GOOBERS_TRIGGER_BACKOFF", 5*time.Second),
		pollLimit:         100,
		exporter:          telemetry.ExporterKind(envOr("GOOBERS_OTEL_EXPORTER", string(telemetry.ExporterStdout))),
		otlpEndpoint:      os.Getenv("GOOBERS_OTLP_ENDPOINT"),
		environment:       os.Getenv("GOOBERS_ENV"),
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg := configFromEnv()
	if cfg.configDir == "" {
		return errors.New("GOOBERS_CONFIG_DIR is required")
	}

	loaded, err := bootstrap.LoadAndRegister(cfg.configDir, cfg.namespace)
	if err != nil {
		return err
	}

	tel, err := telemetry.New(ctx, telemetry.Config{
		ServiceName:    "scheduler",
		ServiceVersion: version.Get().Version,
		Environment:    cfg.environment,
		Exporter:       cfg.exporter,
		OTLPEndpoint:   cfg.otlpEndpoint,
		OTLPInsecure:   cfg.otlpEndpoint != "",
		Batch:          true,
	})
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
	secretReg := journal.NewRegistryScrubber()

	var wg sync.WaitGroup
	for _, g := range loaded.Gaggles {
		provider, repo, perr := bootstrap.BacklogProviderFor(g.Spec.Backlog, cfg.backlogToken, secretReg)
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

		for _, wfName := range workflows {
			tk := time.NewTicker(cfg.pollInterval)
			tr := scheduler.BacklogPollTrigger{
				WorkflowName: wfName,
				Provider:     provider,
				Repo:         repo,
				Labels:       g.Spec.Backlog.Labels,
				Ticks:        tk.C,
				Limit:        cfg.pollLimit,
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
