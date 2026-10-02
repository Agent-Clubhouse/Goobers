package telemetry

func (h *ExporterHealth) destination(name, mode, endpoint string) *ExporterHealth {
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
