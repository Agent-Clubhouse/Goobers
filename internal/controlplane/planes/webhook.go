package planes

import (
	"context"
	"fmt"
	"log"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/secretstore"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

const (
	webhookSecretRefName = "webhook.secret"
	webhookReadTimeout   = 10 * time.Second
)

// WebhookServerConfig supplies the webhook plane's explicit dependencies.
type WebhookServerConfig struct {
	Address        string
	Config         *instance.Config
	Definitions    *instance.ConfigSet
	SecretStores   *secretstore.Registry
	SharedRegistry *journal.RegistryScrubber
	InstanceLog    *journal.InstanceLog
	Scheduler      *localscheduler.Scheduler
	Gate           *webhookhttp.DispatchGate
	ErrorLog       *log.Logger
	ReconcileHook  func(context.Context)
}

// HasWebhookTriggers reports whether any workflow accepts webhook events.
func HasWebhookTriggers(set *instance.ConfigSet) bool {
	for i := range set.Workflows {
		for _, trigger := range set.Workflows[i].Spec.Triggers {
			if trigger.Type == apiv1.TriggerWebhook {
				return true
			}
		}
	}
	return false
}

// WebhookListenerEnabled reports whether configuration requires a listener.
func WebhookListenerEnabled(set *instance.ConfigSet, cfg *instance.Config) bool {
	hasGitSource := cfg.WorkflowSource != nil && cfg.WorkflowSource.Kind == instance.WorkflowSourceKindGit
	return (HasWebhookTriggers(set) || hasGitSource) && cfg.WebhookSecretConfigured()
}

// WebhookListenerTopologyChanged reports whether a reload would add or remove the listener.
func WebhookListenerTopologyChanged(current, next *instance.ConfigSet, cfg *instance.Config) bool {
	return WebhookListenerEnabled(current, cfg) != WebhookListenerEnabled(next, cfg)
}

// WebhookConfigurationWarning describes a trigger configuration disabled by a missing secret.
func WebhookConfigurationWarning(set *instance.ConfigSet, cfg *instance.Config) string {
	if HasWebhookTriggers(set) && !cfg.WebhookSecretConfigured() {
		return "warning: webhook triggers are configured but instance webhook.secret is not; the webhook listener is disabled"
	}
	return ""
}

// BuildWebhookServer constructs the webhook listener when configuration enables it.
func BuildWebhookServer(ctx context.Context, cfg WebhookServerConfig) (*httpapi.Server, error) {
	hasGitSource := cfg.Config.WorkflowSource != nil && cfg.Config.WorkflowSource.Kind == instance.WorkflowSourceKindGit
	if !WebhookListenerEnabled(cfg.Definitions, cfg.Config) {
		return nil, nil
	}
	resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{
		cfg.Config.Webhook.Secret.CredentialTokenRef(webhookSecretRefName),
	}, cfg.SecretStores)
	if err != nil {
		return nil, fmt.Errorf("build webhook credential resolver: %w", err)
	}
	secret, err := resolver.Resolve(ctx, webhookSecretRefName)
	if err != nil {
		return nil, fmt.Errorf("resolve webhook secret: %w", err)
	}
	cfg.SharedRegistry.Register([]byte(secret))
	handlerOpts := []webhookhttp.HandlerOption{webhookhttp.WithDurableSignaler(cfg.Scheduler)}
	if hasGitSource {
		handlerOpts = append(handlerOpts, webhookhttp.WithPushHook(cfg.ReconcileHook))
	}
	handler, err := webhookhttp.NewHandler([]byte(secret), cfg.Scheduler, cfg.InstanceLog, cfg.Gate, handlerOpts...)
	if err != nil {
		return nil, fmt.Errorf("initialize webhook handler: %w", err)
	}
	server, err := httpapi.NewServer(cfg.Address, handler, cfg.ErrorLog, httpapi.WithReadTimeout(webhookReadTimeout))
	if err != nil {
		return nil, fmt.Errorf("initialize webhook listener: %w", err)
	}
	return server, nil
}
