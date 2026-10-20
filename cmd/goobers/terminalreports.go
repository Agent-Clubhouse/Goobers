package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/escalationnotify"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/providers"
)

// Reports outlive claim release and daemon restarts. Only credential-free
// intents live here; each replay resolves current credentials/configuration.
func newTerminalReportQueue(l instance.Layout) *terminalreport.Queue {
	return &terminalreport.Queue{Dir: filepath.Join(l.SchedulerDir(), "terminal-reports"), Gaggle: l.Gaggle(), Log: func(ev journal.Event) error {
		log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
		if err != nil {
			return err
		}
		defer func() { _ = log.Close() }()
		return log.Append(ev)
	}}
}

func startTerminalReportRetry(ctx context.Context, l instance.Layout, registrar terminalSecretRegistry, stores credentials.StoreResolver, log *journal.InstanceLog, ready bool) <-chan struct{} {
	done := make(chan struct{})
	if !ready {
		close(done)
		return done
	}
	q := newTerminalReportQueue(l)
	q.Log = log.Append
	reporter := newSweepErrorReporter(log, "terminal_report_retry_failed")
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			pass, cancel := context.WithTimeout(ctx, 2*time.Minute)
			err := q.Reconcile(pass, 8, func(ctx context.Context, e terminalreport.Effect) error {
				return replayTerminalReport(ctx, l, registrar, stores, e)
			})
			cancel()
			reporter.report(err)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

func replayTerminalReport(ctx context.Context, l instance.Layout, registrar terminalSecretRegistry, stores credentials.StoreResolver, e terminalreport.Effect) error {
	scoped := l.ForGaggle(e.Gaggle)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return err
	}
	project := apiv1.RepoRef{Provider: apiv1.Provider(e.Repository.Provider), Owner: e.Repository.Owner, Project: e.Repository.Project, Name: e.Repository.Name}
	if e.Kind == terminalreport.AbortLabel {
		return replayTerminalAbortLabel(ctx, scoped, cfg, registrar, stores, project, e)
	}
	resolver, _, err := buildCredentials(cfg, stores, credentialBindingOwner(project), project.Name, nil, registrar)
	if err != nil {
		return err
	}
	if e.Attribution.Run != "" {
		ctx = providers.WithAttributionContext(ctx, e.Attribution)
	} else {
		ctx, err = escalationnotify.AttributionContextForRun(ctx, scoped.RunsDir(), e.RunID, "terminal-report")
		if err != nil {
			return err
		}
	}
	poster := &escalationCommenter{resolver: resolver, reg: registrar, layout: scoped, needsHumanAssignee: cfg.NeedsHumanAssignee}
	return e.Apply(ctx, poster)
}

func replayTerminalAbortLabel(ctx context.Context, l instance.Layout, cfg *instance.Config, registrar terminalSecretRegistry, stores credentials.StoreResolver, project apiv1.RepoRef, e terminalreport.Effect) error {
	reader, err := journal.OpenReadOnly(filepath.Join(l.RunsDir(), e.RunID))
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	phase := journal.PhaseFromEvents(events)
	if phase == journal.PhaseCompleted || terminalFailureWasInfrastructure(events) {
		return terminalreport.ErrSuperseded
	}
	if phase != journal.PhaseAborted && phase != journal.PhaseFailed && phase != journal.PhaseEscalated {
		// A runner may persist the intent before appending run.finished.
		// Keep it pending until finalization/recovery establishes the outcome.
		return errors.New("terminal report: originating run is not terminal yet")
	}
	label, err := buildTerminalRunAbortLabeler(cfg, project, registrar, stores)
	if err != nil {
		return err
	}
	if label == nil {
		return errors.New("terminal report: repository credentials unavailable")
	}
	_, err = label(ctx, providers.UpdateWorkItemRequest{Repository: e.Repository, ID: e.ItemID, AddLabels: []string{abortedRunLabel}})
	return err
}
