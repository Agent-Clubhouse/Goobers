package planes

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

func TestWebhookListenerConfiguration(t *testing.T) {
	without := &instance.ConfigSet{}
	with := &instance.ConfigSet{Workflows: []apiv1.Workflow{{
		Spec: apiv1.WorkflowSpec{Triggers: []apiv1.Trigger{{Type: apiv1.TriggerWebhook, Events: []string{"issues"}}}},
	}}}
	configured := &instance.Config{Webhook: instance.WebhookConfig{Secret: instance.TokenRef{Env: "WEBHOOK_SECRET"}}}

	if HasWebhookTriggers(with) != true || HasWebhookTriggers(without) {
		t.Fatal("webhook trigger detection disagrees with definitions")
	}
	if WebhookListenerEnabled(with, &instance.Config{}) {
		t.Fatal("listener enabled without a secret")
	}
	if !WebhookListenerEnabled(with, configured) {
		t.Fatal("listener disabled with a trigger and secret")
	}
	if !WebhookListenerTopologyChanged(without, with, configured) {
		t.Fatal("adding the first webhook trigger did not change listener topology")
	}
	if warning := WebhookConfigurationWarning(with, &instance.Config{}); !strings.Contains(warning, "webhook.secret") {
		t.Fatalf("missing-secret warning = %q", warning)
	}
	if warning := WebhookConfigurationWarning(without, &instance.Config{}); warning != "" {
		t.Fatalf("trigger-free warning = %q", warning)
	}
}

func TestGitWorkflowSourceEnablesWebhookListener(t *testing.T) {
	cfg := &instance.Config{
		WorkflowSource: &instance.WorkflowSource{Kind: instance.WorkflowSourceKindGit},
		Webhook:        instance.WebhookConfig{Secret: instance.TokenRef{Env: "WEBHOOK_SECRET"}},
	}
	if !WebhookListenerEnabled(&instance.ConfigSet{}, cfg) {
		t.Fatal("git workflow source did not enable webhook listener")
	}
}

func TestBuildWebhookServerResolvesAndRegistersSecret(t *testing.T) {
	if server, err := BuildWebhookServer(t.Context(), WebhookServerConfig{
		Config: &instance.Config{}, Definitions: &instance.ConfigSet{},
	}); err != nil || server != nil {
		t.Fatalf("disabled server = %v, %v", server, err)
	}

	const secretEnv = "GOOBERS_PLANES_WEBHOOK_SECRET"
	definitions := &instance.ConfigSet{Workflows: []apiv1.Workflow{{
		Spec: apiv1.WorkflowSpec{Triggers: []apiv1.Trigger{{Type: apiv1.TriggerWebhook}}},
	}}}
	cfg := &instance.Config{Webhook: instance.WebhookConfig{
		Listen: "127.0.0.1:0",
		Secret: instance.TokenRef{Env: secretEnv},
	}}
	t.Setenv(secretEnv, "")
	if _, err := BuildWebhookServer(t.Context(), WebhookServerConfig{
		Address: cfg.Webhook.Listen, Config: cfg, Definitions: definitions,
	}); err == nil || !strings.Contains(err.Error(), "resolve webhook secret:") {
		t.Fatalf("missing-secret error = %v", err)
	}

	const secret = "package-local-webhook-secret"
	t.Setenv(secretEnv, secret)
	registry := journal.NewRegistryScrubber()
	instanceLog, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	gate, err := webhookhttp.NewDispatchGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gate.Stop)
	scheduler := localscheduler.New(nil, instanceLog)
	server, err := BuildWebhookServer(t.Context(), WebhookServerConfig{
		Address: cfg.Webhook.Listen, Config: cfg, Definitions: definitions,
		SharedRegistry: registry, InstanceLog: instanceLog, Scheduler: scheduler,
		Gate: gate, ErrorLog: log.New(io.Discard, "", 0), ReconcileHook: func(context.Context) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if server == nil {
		t.Fatal("enabled webhook server is nil")
	}
	if !strings.Contains(string(registry.Scrub([]byte(secret))), journal.Redacted) {
		t.Fatal("webhook secret was not registered with the scrubber")
	}
}
