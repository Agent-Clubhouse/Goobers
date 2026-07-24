package main

import (
	"context"
	"fmt"
	"log"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

const (
	webhookSecretRefName = "webhook.secret"
	webhookReadTimeout   = 10 * time.Second
)

var webhookListenAddress = func(c *instance.Config) string { return c.WebhookListenAddress() }

func hasWebhookTriggers(set *instance.ConfigSet) bool {
	for i := range set.Workflows {
		for _, trigger := range set.Workflows[i].Spec.Triggers {
			if trigger.Type == apiv1.TriggerWebhook {
				return true
			}
		}
	}
	return false
}

func webhookListenerTopologyChanged(current, next *instance.ConfigSet) bool {
	return hasWebhookTriggers(current) != hasWebhookTriggers(next)
}

func webhookConfigurationWarning(set *instance.ConfigSet, cfg *instance.Config) string {
	if hasWebhookTriggers(set) && !cfg.WebhookSecretConfigured() {
		return "warning: webhook triggers are configured but instance webhook.secret is not; the webhook listener is disabled"
	}
	return ""
}

func buildWebhookServer(ctx context.Context, setup *schedulerSetup, sched *localscheduler.Scheduler, gate *webhookhttp.DispatchGate, errorLog *log.Logger) (*httpapi.Server, error) {
	if !hasWebhookTriggers(setup.Definitions) || !setup.Config.WebhookSecretConfigured() {
		return nil, nil
	}
	env, file, err := setup.Config.Webhook.Secret.EnvFileSources()
	if err != nil {
		return nil, fmt.Errorf("build webhook credential resolver: %w", err)
	}
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{
		Name: webhookSecretRefName,
		Env:  env,
		File: file,
	}})
	if err != nil {
		return nil, fmt.Errorf("build webhook credential resolver: %w", err)
	}
	secret, err := resolver.Resolve(ctx, webhookSecretRefName)
	if err != nil {
		return nil, fmt.Errorf("resolve webhook secret: %w", err)
	}
	setup.SharedRegistry.Register([]byte(secret))
	handler, err := webhookhttp.NewHandler([]byte(secret), sched, setup.InstanceLog, gate)
	if err != nil {
		return nil, fmt.Errorf("initialize webhook handler: %w", err)
	}
	server, err := httpapi.NewServer(webhookListenAddress(setup.Config), handler, errorLog, httpapi.WithReadTimeout(webhookReadTimeout))
	if err != nil {
		return nil, fmt.Errorf("initialize webhook listener: %w", err)
	}
	return server, nil
}
