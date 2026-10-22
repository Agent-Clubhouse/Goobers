package planes

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
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
