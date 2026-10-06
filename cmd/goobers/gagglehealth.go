package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gagglehealth"
	"github.com/goobers/goobers/internal/instance"
)

type daemonGaggleHealth struct {
	mu          sync.RWMutex
	definitions *instance.ConfigSet
	retentions  map[string]time.Duration
	store       *gagglehealth.Store
	controller  *gagglehealth.Controller
}

func startDaemonGaggleHealth(ctx context.Context, root string, definitions *instance.ConfigSet) (*daemonGaggleHealth, error) {
	health := &daemonGaggleHealth{}
	health.replaceDefinitions(definitions)
	store, err := gagglehealth.OpenStore(root, health.retention)
	if err != nil {
		return nil, err
	}
	controller, err := gagglehealth.NewController(store, health, gagglehealth.ControllerOptions{MaxConcurrent: 2})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	health.store = store
	health.controller = controller
	if err := controller.Start(ctx, health.registrations(definitions)); err != nil {
		_ = store.Close()
		return nil, err
	}
	return health, nil
}

func (h *daemonGaggleHealth) Replace(definitions *instance.ConfigSet) error {
	if h == nil {
		return nil
	}
	registrations := h.registrations(definitions)
	h.replaceDefinitions(definitions)
	if err := h.controller.Reload(registrations); err != nil {
		return fmt.Errorf("reload gaggle health controllers: %w", err)
	}
	return nil
}

func (h *daemonGaggleHealth) Close() error {
	if h == nil {
		return nil
	}
	h.controller.Stop()
	return h.store.Close()
}

func (h *daemonGaggleHealth) Health(_ context.Context, gaggle string) (apiv1.GaggleHealthResponse, error) {
	snapshot, err := h.store.Snapshot(gaggle)
	if err != nil {
		return apiv1.GaggleHealthResponse{}, err
	}
	status, ok := h.controller.Status(gaggle)
	if !ok {
		return apiv1.GaggleHealthResponse{}, fmt.Errorf("gaggle %q health controller is not active", gaggle)
	}
	controller := &apiv1.GaggleHealthControllerStatus{
		FreshAt:            status.FreshAt,
		LastError:          status.LastError,
		NextEvaluation:     status.NextEvaluation,
		ActiveFindingCount: status.ActiveFindings,
		Evaluating:         status.Evaluating,
	}
	if !status.LastSuccessfulEvaluation.IsZero() {
		lastSuccess := status.LastSuccessfulEvaluation
		controller.LastSuccessfulEvaluation = &lastSuccess
	}
	return apiv1.GaggleHealthResponse{
		SchemaVersion: apiv1.GaggleHealthSchemaVersion,
		Health:        snapshot,
		Controller:    controller,
	}, nil
}

func (h *daemonGaggleHealth) Snapshot(_ context.Context, gaggle string, _ []gagglehealth.EvidenceDependency) (gagglehealth.Snapshot, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.definitions == nil {
		return gagglehealth.Snapshot{}, errors.New("gaggle health definitions unavailable")
	}
	snapshot := gagglehealth.Snapshot{Gaggle: gaggle}
	found := false
	for _, configured := range h.definitions.Gaggles {
		if configured.Name == gaggle {
			found = true
			break
		}
	}
	if !found {
		return gagglehealth.Snapshot{}, fmt.Errorf("gaggle %q is not loaded", gaggle)
	}
	for _, workflow := range h.definitions.Workflows {
		if workflow.Spec.Gaggle == gaggle {
			snapshot.Workflows = append(snapshot.Workflows, workflow.Name)
		}
	}
	return snapshot, nil
}

func (h *daemonGaggleHealth) retention(gaggle string) (time.Duration, error) {
	h.mu.RLock()
	retention := h.retentions[gaggle]
	h.mu.RUnlock()
	if retention == 0 {
		retention, _ = time.ParseDuration(gagglehealth.DefaultPolicy().Thresholds.EvidenceRetention)
	}
	return retention, nil
}

func (h *daemonGaggleHealth) replaceDefinitions(definitions *instance.ConfigSet) {
	retentions := make(map[string]time.Duration)
	if definitions != nil {
		for _, configured := range definitions.Gaggles {
			policy, err := gagglehealth.ResolvePolicy(configured.Spec.Health)
			if err != nil {
				continue
			}
			retention, _ := time.ParseDuration(policy.Thresholds.EvidenceRetention)
			retentions[configured.Name] = retention
		}
	}
	h.mu.Lock()
	h.definitions = definitions
	h.retentions = retentions
	h.mu.Unlock()
}

func (h *daemonGaggleHealth) registrations(definitions *instance.ConfigSet) []gagglehealth.GaggleRegistration {
	if definitions == nil {
		return nil
	}
	registrations := make([]gagglehealth.GaggleRegistration, 0, len(definitions.Gaggles))
	for _, configured := range definitions.Gaggles {
		policy, err := gagglehealth.ResolvePolicy(configured.Spec.Health)
		if err != nil {
			continue
		}
		registrations = append(registrations, gagglehealth.GaggleRegistration{Name: configured.Name, Policy: policy})
	}
	return registrations
}

var _ gagglehealth.SnapshotSource = (*daemonGaggleHealth)(nil)
