package eventing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Consumer         string `json:"consumer"`
	Revision         string `json:"revision"`
	Workflow         string `json:"workflow"`
	WorkflowDigest   string `json:"workflowDigest"`
	GooberDigest     string `json:"gooberDigest"`
	ConfigGeneration string `json:"configGeneration"`
	// FailureReason is a bounded service-authored delivery refusal, for example
	// a missing evaluated debounce key. It never echoes raw payload content.
	FailureReason string    `json:"failureReason,omitempty"`
	Debounce      *Debounce `json:"debounce,omitempty"`
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
	for _, value := range []string{r.Consumer, r.Revision, r.Workflow, r.WorkflowDigest, r.GooberDigest, r.ConfigGeneration} {
		if !boundedText(value, 256) {
			return errors.New("eventing: route must pin consumer, revision and workflow digests")
		}
	}
	if r.FailureReason != "" {
		if !boundedText(r.FailureReason, 256) {
			return errors.New("eventing: invalid delivery refusal")
		}
		return nil
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

// ParsePlan validates an immutable routing snapshot without consulting current
// configuration. Missing pins, duplicate keys and unknown fields fail closed.
func ParsePlan(raw []byte) (Plan, error) {
	var plan Plan
	if err := decodeClosed(raw, MaxRoutingBytes, &plan); err != nil {
		return plan, err
	}
	_, err := plan.Marshal()
	return plan, err
}

func decodeClosed(raw []byte, limit int, target any) error {
	if len(raw) == 0 || len(raw) > limit {
		return errors.New("eventing: invalid retained document size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if _, err := decodeValue(decoder, 0); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("eventing: multiple documents")
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("eventing: multiple documents")
	}
	return nil
}

func boundedText(text string, limit int) bool {
	if text == "" || strings.TrimSpace(text) != text || len(text) > limit {
		return false
	}
	return attributeScalar(text) == nil
}
