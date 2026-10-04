package eventing

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// MaxConsumers bounds transactional fan-out for one event.
	MaxConsumers = 32
	// MaxRoutingBytes bounds the immutable route snapshot retained at intake.
	MaxRoutingBytes = 16 << 10
)

// Route is a matched consumer, after evaluating the pinned subscription filter.
// Key is already evaluated from that event; a missing key cannot join a global
// empty-key bucket. All targets inherit the accepting gaggle, with no override.
type Route struct {
	Consumer       string    `json:"consumer"`
	Revision       string    `json:"revision"`
	Workflow       string    `json:"workflow"`
	WorkflowDigest string    `json:"workflowDigest"`
	GooberDigest   string    `json:"gooberDigest"`
	Debounce       *Debounce `json:"debounce,omitempty"`
}

// Debounce uses server receipt time and retains all original memberships even
// when InputMode is latest. Duration values serialize as nanoseconds internally.
type Debounce struct {
	Key       string        `json:"key"`
	Window    time.Duration `json:"window"`
	MaxWait   time.Duration `json:"maxWait"`
	MaxEvents int           `json:"maxEvents"`
	InputMode string        `json:"inputMode"`
}

// Plan pins the route decisions at acceptance. An empty Routes slice means a
// successful publication without consumers; it is never an unknown-signal error.
type Plan struct {
	Revision string  `json:"revision"`
	Routes   []Route `json:"routes"`
}

// Marshal validates and freezes a matched routing plan. Revisions are opaque
// bounded identities; workflow/goober digests retain the repository's format.
func (p Plan) Marshal() ([]byte, error) {
	if !boundedText(p.Revision, 256) || len(p.Routes) > MaxConsumers {
		return nil, errors.New("eventing: invalid routing revision or fan-out")
	}
	seen := make(map[string]bool)
	for _, route := range p.Routes {
		if err := route.validate(); err != nil {
			return nil, err
		}
		if seen[route.Consumer] {
			return nil, errors.New("eventing: duplicate consumer in routing plan")
		}
		seen[route.Consumer] = true
	}
	if p.Routes == nil {
		p.Routes = []Route{}
	}
	data, err := json.Marshal(p)
	if err != nil || len(data) > MaxRoutingBytes {
		return nil, errors.New("eventing: routing plan exceeds size limit")
	}
	return data, nil
}

func (r Route) validate() error {
	for _, value := range []string{r.Consumer, r.Revision, r.Workflow, r.WorkflowDigest, r.GooberDigest} {
		if !boundedText(value, 256) {
			return errors.New("eventing: route must pin consumer, revision and workflow digests")
		}
	}
	if r.Debounce == nil {
		return nil
	}
	d := r.Debounce
	if !boundedText(d.Key, 256) || d.Window < 100*time.Millisecond || d.Window > 5*time.Minute || d.MaxWait < d.Window || d.MaxWait > time.Hour || d.MaxEvents < 1 || d.MaxEvents > 1000 || (d.InputMode != "all" && d.InputMode != "latest") {
		return fmt.Errorf("eventing: invalid debounce for consumer %q", r.Consumer)
	}
	return nil
}

func boundedText(text string, limit int) bool {
	if text == "" || strings.TrimSpace(text) != text || len(text) > limit {
		return false
	}
	return attributeScalar(text) == nil
}
