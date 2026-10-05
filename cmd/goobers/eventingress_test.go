package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/httpapi"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func externalEventHost(t *testing.T, matched bool) (*eventHostFixture, http.Handler) {
	t.Helper()
	f := eventHostConfigured(t, func(f *eventHostFixture, source string) string {
		path := filepath.Join(f.layout.ConfigDir(), "gaggles/example/gaggle.yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		kind := "build.finished"
		if !matched {
			kind = "different"
		}
		policy := "  events:\n    ingress:\n      - name: builds\n        issuer: https://identity.example\n        subject: build-system\n        source: urn:builds\n        allowedTypes: [build.finished]\n    subscriptions:\n      - name: consumer\n        workflow: default-implement\n        filter:\n          all: [{attribute: type, equals: " + kind + "}]\n"
		writeFileContent(t, path, strings.Replace(string(raw), "spec:\n", "spec:\n"+policy, 1))
		return source
	})
	set, report, err := loadConfigDirectory(f.layout.ConfigDir())
	if err != nil {
		t.Fatal(err, report)
	}
	// No human policy or provider credential is needed for the explicitly bound producer.
	f.setup.InteractiveAccess, err = interactiveaccess.New(set.Gaggles, nil, interactiveaccess.Dependencies{Registrar: journal.NewRegistryScrubber()})
	if err != nil {
		t.Fatal(err)
	}
	u := &upSession{}
	u.setup = f.setup
	u.durableTriggers = f.service
	u.configureEventIngress()
	principal := httpapi.Principal{Issuer: "https://identity.example", Subject: "build-system", Roles: []httpapi.Role{httpapi.RoleOperate}}
	opts := append(u.apiHandlerOpts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &principal}))
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return f, handler
}
func postExternalEvent(t *testing.T, h http.Handler, id string) (apicontract.GaggleEventReceipt, int) {
	t.Helper()
	raw := `{"specversion":"1.0","id":"` + id + `","source":"urn:builds","type":"build.finished","data":{"rootId":"untrusted","text":"source content"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gaggles/example/events", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set(apicontract.EventBindingHeader, "builds")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	var result apicontract.GaggleEventReceipt
	if response.Code == http.StatusAccepted {
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Log(response.Code, response.Body.String())
	}
	return result, response.Code
}

func TestExternalEventHTTPReceiptPinnedConsumerAndLostReply(t *testing.T) {
	for _, matched := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmatched", true: "consumer"}[matched], func(t *testing.T) {
			f, h := externalEventHost(t, matched)
			first, status := postExternalEvent(t, h, "one")
			if status != 202 || first.Duplicate {
				t.Fatal(first, status)
			}
			// Simulate the client losing the committed reply, then retry after mutable edits.
			writeFileContent(t, f.source, "invalid pending YAML: [")
			duplicate, status := postExternalEvent(t, h, "one")
			if status != 202 || !duplicate.Duplicate || duplicate.ReceiptID != first.ReceiptID {
				t.Fatal(duplicate, status)
			}
			receipt, err := f.service.queue.EventInGaggle(t.Context(), "example", first.ReceiptID)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Producer.RunID != "" || receipt.Producer.RootID != "" || receipt.Producer.Binding != "ingress:builds" {
				t.Fatal("forged lineage", receipt.Producer)
			}
			if !matched {
				if first.State != "accepted_unmatched" {
					t.Fatal(first)
				}
				return
			}
			f.now = time.Now().UTC()
			for range 4 {
				f.drain(t)
				f.sched.Wait()
				f.wg.Wait()
			}
			record, start := f.record(t, receipt)
			if record.State != triggerqueue.Dispatched || start.ConfigGeneration != f.generation {
				t.Fatal(record, start)
			}
			dir, err := f.layout.FindRunDir(record.RunID)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := journal.OpenReadOnly(dir)
			if err != nil {
				t.Fatal(err)
			}
			phase, err := reader.Phase()
			if err != nil || phase != journal.PhaseCompleted {
				t.Fatal(phase, err)
			}
			id, err := reader.Identity()
			if err != nil || id.ConfigGeneration != f.generation || id.WorkflowDigest != f.entry.WorkflowDigest || id.Event == nil {
				t.Fatal(id, err)
			}
		})
	}
}

func TestExternalEventAppliedReloadRevokesProducerWithoutReroutingReceipt(t *testing.T) {
	f, h := externalEventHost(t, true)
	first, status := postExternalEvent(t, h, "one")
	if status != 202 {
		t.Fatal(status)
	}
	publisher := f.setup.EventPublisher
	next := publisher.snapshot
	next.policies = map[string]*apiv1.GaggleEvents{}
	reloader := &configReloader{setup: f.setup}
	failed := errors.New("publication failed")
	if err := reloader.publishEventDefinitions(next, func() error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if _, status = postExternalEvent(t, h, "two"); status != 202 {
		t.Fatal("failed reload changed binding", status)
	}
	if err := reloader.publishEventDefinitions(next, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, status = postExternalEvent(t, h, "three"); status != 403 {
		t.Fatal("revoked producer accepted", status)
	}
	retained, err := f.service.queue.EventInGaggle(t.Context(), "example", first.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.State != triggerqueue.EventRoutingPending || len(retained.Plan) == 0 {
		t.Fatal("revocation discarded custody", retained)
	}
	dependencies, err := f.service.queue.EventDependencyPage(t.Context(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(dependencies) == 0 || len(dependencies[0].ConfigGenerations) == 0 {
		t.Fatal("accepted generation unretained", dependencies)
	}
}

func TestExternalEventAcceptanceFencesAppliedPolicyReload(t *testing.T) {
	f, _ := externalEventHost(t, false)
	publisher := f.setup.EventPublisher
	next := publisher.snapshot
	next.policies = map[string]*apiv1.GaggleEvents{}
	entered, release := make(chan struct{}), make(chan struct{})
	accepted := make(chan error, 1)
	p := httpapi.Principal{Issuer: "https://identity.example", Subject: "build-system", Roles: []httpapi.Role{httpapi.RoleOperate}}
	go func() {
		accepted <- publisher.authorizeIngress(context.Background(), p, "example", "builds", func(b apiv1.EventIngressBinding, catalog *eventing.Catalog) error {
			close(entered)
			<-release
			if b.Source != "urn:builds" || catalog == nil {
				return errors.New("missing applied scope")
			}
			return nil
		})
	}()
	<-entered
	reloaded := make(chan error, 1)
	go func() {
		r := &configReloader{setup: f.setup}
		reloaded <- r.publishEventDefinitions(next, func() error { return nil })
	}()
	select {
	case err := <-reloaded:
		close(release)
		<-accepted
		t.Fatalf("reload passed active acceptance: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
	if err := publisher.authorizeIngress(t.Context(), p, "example", "builds", func(apiv1.EventIngressBinding, *eventing.Catalog) error {
		t.Fatal("revoked producer reached acceptance")
		return nil
	}); err == nil {
		t.Fatal("revocation was not published")
	}
}

func TestExternalEventAuthorizationHonorsDeadlineWhileReloadOwnsSnapshot(t *testing.T) {
	f, _ := externalEventHost(t, false)
	publisher := f.setup.EventPublisher
	publisher.mu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- publisher.authorizeIngress(ctx, httpapi.Principal{}, "example", "builds", func(apiv1.EventIngressBinding, *eventing.Catalog) error {
			return errors.New("cancelled request reached acceptance")
		})
	}()
	select {
	case err := <-done:
		publisher.mu.Unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		publisher.mu.Unlock()
		<-done
		t.Fatal("event authorization exceeded its request deadline waiting for reload")
	}
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	if err := publisher.lockIngressSnapshot(cancelled); !errors.Is(err, context.Canceled) {
		if err == nil {
			publisher.mu.RUnlock()
		}
		t.Fatal("already-cancelled request acquired a snapshot", err)
	}
}
