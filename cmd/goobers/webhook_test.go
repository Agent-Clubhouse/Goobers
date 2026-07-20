package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

func configureWebhook(t *testing.T, root, address, secretEnv string) {
	t.Helper()
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Webhook.Listen = address
	cfg.Webhook.Secret.Env = secretEnv
	if err := instance.WriteConfig(l.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestUpDoesNotBindWebhookListenerWithoutWebhookTriggers(t *testing.T) {
	previousReloadInterval := configReloadInterval
	configReloadInterval = 20 * time.Millisecond
	t.Cleanup(func() { configReloadInterval = previousReloadInterval })

	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	address := freeLoopbackAddress(t)
	const secretEnv = "GOOBERS_TEST_UNUSED_WEBHOOK_SECRET"
	t.Setenv(secretEnv, "configured-but-unused-secret")
	configureWebhook(t, root, address, secretEnv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := &daemonStartedWriter{started: make(chan struct{})}
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runUpContext(ctx, []string{"--quiet", "--watch-config", root}, started, &stderr)
	}()
	select {
	case <-started.started:
	case code := <-done:
		t.Fatalf("daemon exited before startup: code = %d, stderr = %q", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("webhook address was bound without webhook triggers: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	webhookWorkflow := strings.Replace(
		deterministicWorkflowYAML,
		"    - type: schedule\n      schedule: \"@every 24h\"\n",
		"    - type: webhook\n      events: [issues]\n",
		1,
	)
	if err := os.WriteFile(workflowPath, []byte(webhookWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	rejected := waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloadRejected, 1)
	if rejected.Error == nil || !strings.Contains(rejected.Error.Message, "requires a daemon restart") {
		t.Fatalf("reload rejection = %+v", rejected.Error)
	}
	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("rejected webhook reload unexpectedly bound a listener: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}

func TestWebhookConfigurationDiagnostics(t *testing.T) {
	without := &instance.ConfigSet{}
	with := &instance.ConfigSet{Workflows: []apiv1.Workflow{{
		Spec: apiv1.WorkflowSpec{Triggers: []apiv1.Trigger{{
			Type: apiv1.TriggerWebhook, Events: []string{"issues"},
		}}},
	}}}
	if warning := webhookConfigurationWarning(with, &instance.Config{}); !strings.Contains(warning, "webhook.secret") {
		t.Fatalf("missing-secret warning = %q", warning)
	}
	if warning := webhookConfigurationWarning(without, &instance.Config{}); warning != "" {
		t.Fatalf("trigger-free webhook warning = %q", warning)
	}
	cfg := &instance.Config{Webhook: instance.WebhookConfig{Secret: instance.TokenRef{Env: "WEBHOOK_SECRET"}}}
	if warning := webhookConfigurationWarning(with, cfg); warning != "" {
		t.Fatalf("configured webhook warning = %q", warning)
	}
}

func TestUpSurfacesWebhookStartupFailure(t *testing.T) {
	root := initDeterministicDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	webhookWorkflow := strings.Replace(
		deterministicWorkflowYAML,
		"    - type: schedule\n      schedule: \"@every 24h\"\n",
		"    - type: webhook\n      events: [issues]\n",
		1,
	)
	if err := os.WriteFile(workflowPath, []byte(webhookWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := occupied.Close(); err != nil {
			t.Errorf("close occupied webhook listener: %v", err)
		}
	})
	const secretEnv = "GOOBERS_TEST_WEBHOOK_STARTUP_SECRET"
	t.Setenv(secretEnv, "webhook-startup-secret")
	configureWebhook(t, root, occupied.Addr().String(), secretEnv)

	var stdout, stderr bytes.Buffer
	code := runUpContext(context.Background(), []string{"--quiet", root}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "start webhook listener") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestUpWebhookAuthenticatesRoutesDeduplicatesAndAppliesReadiness(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	webhookWorkflow := strings.Replace(
		deterministicWorkflowYAML,
		"    - type: schedule\n      schedule: \"@every 24h\"\n",
		"    - type: webhook\n      events: [issues]\n  readiness:\n    maxRunsPerHour: 1\n",
		1,
	)
	if webhookWorkflow == deterministicWorkflowYAML {
		t.Fatal("deterministic workflow fixture did not contain expected schedule trigger")
	}
	if err := os.WriteFile(workflowPath, []byte(webhookWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}

	address := freeLoopbackAddress(t)
	const (
		secretEnv = "GOOBERS_TEST_WEBHOOK_SECRET"
		secret    = "end-to-end-webhook-secret"
	)
	t.Setenv(secretEnv, secret)
	configureWebhook(t, root, address, secretEnv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := &daemonStartedWriter{started: make(chan struct{})}
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runUpContext(ctx, []string{"--quiet", root}, started, &stderr)
	}()
	select {
	case <-started.started:
	case code := <-done:
		t.Fatalf("daemon exited before startup: code = %d, stderr = %q", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	body := []byte(`{"action":"labeled"}`)
	if status := postWebhook(t, address, "wrong-secret", "issues", "invalid-1", body); status != http.StatusUnauthorized {
		t.Fatalf("invalid signature status = %d, want %d", status, http.StatusUnauthorized)
	}
	if entries, err := os.ReadDir(l.ForGaggle("example").RunsDir()); err != nil || len(entries) != 0 {
		t.Fatalf("runs after invalid delivery = %v, err = %v", entries, err)
	}

	start := time.Now()
	if status := postWebhook(t, address, secret, "issues", "delivery-1", body); status != http.StatusAccepted {
		t.Fatalf("valid delivery status = %d, want %d", status, http.StatusAccepted)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("valid delivery took %s, want at most 1s", elapsed)
	}

	var runID string
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventRunStarted && event.Workflow == "default-implement" {
			runID = event.RunID
		}
	}
	if runID == "" {
		t.Fatalf("valid delivery did not dispatch a run: %+v", events)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	phase, err := waitForRunTerminal(waitCtx, l.ForGaggle("example").RunsDir(), runID)
	waitCancel()
	if err != nil {
		t.Fatalf("wait for webhook run: %v", err)
	}
	if phase != journal.PhaseCompleted {
		t.Fatalf("webhook run phase = %s, want %s", phase, journal.PhaseCompleted)
	}

	if status := postWebhook(t, address, secret, "issues", "delivery-1", body); status != http.StatusAccepted {
		t.Fatalf("replayed delivery status = %d, want %d", status, http.StatusAccepted)
	}

	deadline := time.Now().Add(2 * time.Second)
	for delivery := 2; ; delivery++ {
		if status := postWebhook(t, address, secret, "issues", fmt.Sprintf("delivery-%d", delivery), body); status != http.StatusAccepted {
			t.Fatalf("readiness-limited delivery status = %d, want %d", status, http.StatusAccepted)
		}
		events, err = journal.ReadInstanceLog(l.SchedulerDir())
		if err != nil {
			t.Fatal(err)
		}
		var budgetSkipped bool
		for _, event := range events {
			if event.Type == journal.EventTickSkipped && event.Workflow == "default-implement" && strings.Contains(event.Reason, "budget") {
				budgetSkipped = true
				break
			}
		}
		if budgetSkipped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("webhook deliveries never reached the hourly readiness budget: %+v", events)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var invalidNotes, starts, budgetSkips int
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == "webhook_signature_invalid" {
			invalidNotes++
		}
		if event.Type == journal.EventRunStarted && event.Workflow == "default-implement" {
			starts++
		}
		if event.Type == journal.EventTickSkipped && event.Workflow == "default-implement" && strings.Contains(event.Reason, "budget") {
			budgetSkips++
		}
	}
	if invalidNotes != 1 || starts != 1 || budgetSkips != 1 {
		t.Fatalf("journal counts: invalid=%d starts=%d budget-skips=%d; events=%+v", invalidNotes, starts, budgetSkips, events)
	}

	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	if _, err := client.Get("http://" + address + webhookhttp.Path); err == nil {
		t.Fatal("webhook listener still accepted requests after daemon shutdown")
	}
}

func postWebhook(t *testing.T, address, secret, event, delivery string, body []byte) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://"+address+webhookhttp.Path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-GitHub-Delivery", delivery)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	status := response.StatusCode
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return status
}
