package telemetry

// Destination returns the independent monitor for a validated destination name.
func (h *ExporterHealth) Destination(name, mode, endpoint string) *ExporterHealth {
	if mode == "otlp-grpc" {
		mode = "otlp"
	}
	if mode == "azuremonitor" {
		mode = "azure-monitor"
	}
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.destinations == nil {
		h.destinations = make(map[string]*ExporterHealth)
	}
	if existing := h.destinations[name]; existing != nil {
		return existing
	}
	child := NewExporterHealth(true, mode, endpoint)
	child.name, child.journal = name, h.journal
	h.destinations[name] = child
	return child
}

func (h *ExporterHealth) destinationMonitors() map[string]*ExporterHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make(map[string]*ExporterHealth, len(h.destinations))
	for name, child := range h.destinations {
		result[name] = child
	}
	return result
}

func aggregateDestinationSignal(destinations map[string]ExporterHealthSnapshot, metric bool) ExporterSignalStatus {
	result := ExporterSignalStatus{State: "disabled"}
	for _, destination := range destinations {
		signal := destination.Trace
		if metric {
			signal = destination.Metric
		}
		if !signal.Configured {
			continue
		}
		result.Configured = true
		if result.State == "disabled" || signal.State == "unhealthy" || signal.State == "unknown" && result.State != "unhealthy" {
			result.State = signal.State
		}
		result.ConsecutiveFailures += signal.ConsecutiveFailures
		result.RecoveryTransitions += signal.RecoveryTransitions
		result.FailureTransitions += signal.FailureTransitions
		result.SuppressedFailureEvents += signal.SuppressedFailureEvents
		if signal.LastSuccessAt != nil && (result.LastSuccessAt == nil || signal.LastSuccessAt.After(*result.LastSuccessAt)) {
			result.LastSuccessAt = signal.LastSuccessAt
		}
		if signal.LastFailureAt != nil && (result.LastFailureAt == nil || signal.LastFailureAt.After(*result.LastFailureAt)) {
			result.LastFailureAt = signal.LastFailureAt
			result.LastFailureReason = signal.LastFailureReason
		}
		if signal.LastTransitionAt != nil && (result.LastTransitionAt == nil || signal.LastTransitionAt.After(*result.LastTransitionAt)) {
			result.LastTransitionAt = signal.LastTransitionAt
		}
	}
	return result
}
