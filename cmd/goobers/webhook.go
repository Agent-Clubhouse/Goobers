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

func webhookListenerEnabled(set *instance.ConfigSet, cfg *instance.Config) bool {
	hasGitSource := cfg.WorkflowSource != nil && cfg.WorkflowSource.Kind == instance.WorkflowSourceKindGit
	return (hasWebhookTriggers(set) || hasGitSource) && cfg.WebhookSecretConfigured()
}

func webhookListenerTopologyChanged(current, next *instance.ConfigSet, cfg *instance.Config) bool {
	return webhookListenerEnabled(current, cfg) != webhookListenerEnabled(next, cfg)
}

func webhookConfigurationWarning(set *instance.ConfigSet, cfg *instance.Config) string {
	if hasWebhookTriggers(set) && !cfg.WebhookSecretConfigured() {
		return "warning: webhook triggers are configured but instance webhook.secret is not; the webhook listener is disabled"
	}
	return ""
}

func buildWebhookServer(ctx context.Context, setup *schedulerSetup, sched *localscheduler.Scheduler, gate *webhookhttp.DispatchGate, errorLog *log.Logger, reconcileHook func(context.Context)) (*httpapi.Server, error) {
	hasGitSource := setup.Config.WorkflowSource != nil && setup.Config.WorkflowSource.Kind == instance.WorkflowSourceKindGit
	if !webhookListenerEnabled(setup.Definitions, setup.Config) {
		return nil, nil
	}
	resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{
		setup.Config.Webhook.Secret.CredentialTokenRef(webhookSecretRefName),
	}, setup.SecretStores)
	if err != nil {
		return nil, fmt.Errorf("build webhook credential resolver: %w", err)
	}
	secret, err := resolver.Resolve(ctx, webhookSecretRefName)
	if err != nil {
		return nil, fmt.Errorf("resolve webhook secret: %w", err)
	}
	setup.SharedRegistry.Register([]byte(secret))
	var handlerOpts []webhookhttp.HandlerOption
	if hasGitSource {
		handlerOpts = append(handlerOpts, webhookhttp.WithPushHook(reconcileHook))
	}
	handler, err := webhookhttp.NewHandler([]byte(secret), sched, setup.InstanceLog, gate, handlerOpts...)
	if err != nil {
		return nil, fmt.Errorf("initialize webhook handler: %w", err)
	}
	server, err := httpapi.NewServer(webhookListenAddress(setup.Config), handler, errorLog, httpapi.WithReadTimeout(webhookReadTimeout))
	if err != nil {
		return nil, fmt.Errorf("initialize webhook listener: %w", err)
	}
	return server, nil
}
