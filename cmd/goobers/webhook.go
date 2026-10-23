package main

import (
	"context"
	"log"

	"github.com/goobers/goobers/internal/controlplane/planes"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

var webhookListenAddress = func(c *instance.Config) string { return c.WebhookListenAddress() }

func webhookListenerTopologyChanged(current, next *instance.ConfigSet, cfg *instance.Config) bool {
	return planes.WebhookListenerTopologyChanged(current, next, cfg)
}

func webhookConfigurationWarning(set *instance.ConfigSet, cfg *instance.Config) string {
	return planes.WebhookConfigurationWarning(set, cfg)
}

func buildWebhookServer(ctx context.Context, setup *schedulerSetup, sched *localscheduler.Scheduler, gate *webhookhttp.DispatchGate, errorLog *log.Logger, reconcileHook func(context.Context)) (*httpapi.Server, error) {
	return planes.BuildWebhookServer(ctx, planes.WebhookServerConfig{
		Address: webhookListenAddress(setup.Config), Config: setup.Config, Definitions: setup.Definitions,
		SecretStores: setup.SecretStores, SharedRegistry: setup.SharedRegistry, InstanceLog: setup.InstanceLog,
		Scheduler: sched, Gate: gate, ErrorLog: errorLog, ReconcileHook: reconcileHook,
	})
}
