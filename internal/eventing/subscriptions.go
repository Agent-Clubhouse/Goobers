package eventing

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MaxSubscriptions bounds matching work in one gaggle. A publication may still
// match at most MaxConsumers; excess fan-out is refused before receipt acceptance.
const MaxSubscriptions = 128

// Filter has exactly one equality, all, any or not expression. The initial
// profile deliberately supports envelope attributes only, not data scripts.
type Filter struct {
	Attribute string   `json:"attribute,omitempty"`
	Equals    string   `json:"equals,omitempty"`
	All       []Filter `json:"all,omitempty"`
	Any       []Filter `json:"any,omitempty"`
	Not       *Filter  `json:"not,omitempty"`
}

// DebouncePolicy selects one declared key source. ConstantKey is an explicit
// shared group; an absent attribute is never silently mapped into that group.
type DebouncePolicy struct {
	KeyAttribute string        `json:"keyAttribute,omitempty"`
	ConstantKey  string        `json:"constantKey,omitempty"`
	Window       time.Duration `json:"window"`
	MaxWait      time.Duration `json:"maxWait"`
	MaxEvents    int           `json:"maxEvents"`
	InputMode    string        `json:"inputMode"`
}

// Subscription is a compiled consumer snapshot supplied by the trusted catalog
// loader. Neither envelope data nor a request body can choose target authority.
type Subscription struct {
	Target   Route           `json:"target"`
	Filter   Filter          `json:"filter"`
	Debounce *DebouncePolicy `json:"debounce,omitempty"`
}

// Catalog owns an immutable, gaggle-local subscription generation.
type Catalog struct {
	gaggle        string
	revision      string
	subscriptions []Subscription
}

// CompileCatalog validates bounded filters and copies caller-owned definitions.
// Target pins must already be resolved against the retained configuration.
func CompileCatalog(gaggle, revision string, subscriptions []Subscription) (*Catalog, error) {
	if !boundedText(gaggle, 128) || !boundedText(revision, 256) || len(subscriptions) > MaxSubscriptions {
		return nil, errors.New("eventing: invalid subscription catalog bounds")
	}
	seen := map[string]bool{}
	for _, sub := range subscriptions {
		if err := validateSubscription(sub); err != nil {
			return nil, err
		}
		if seen[sub.Target.Consumer] {
			return nil, errors.New("eventing: duplicate subscription")
		}
		seen[sub.Target.Consumer] = true
	}
	raw, err := json.Marshal(subscriptions)
	if err != nil || len(raw) > 128<<10 {
		return nil, errors.New("eventing: subscription catalog exceeds bound")
	}
	var copied []Subscription
	if err = json.Unmarshal(raw, &copied); err != nil {
		return nil, err
	}
	return &Catalog{gaggle: gaggle, revision: revision, subscriptions: copied}, nil
}

func validateSubscription(sub Subscription) error {
	if sub.Target.Debounce != nil || sub.Target.FailureReason != "" {
		return errors.New("eventing: configured target cannot supply evaluated delivery state")
	}
	if err := sub.Target.validate(); err != nil {
		return err
	}
	remaining := 64
	if err := validateFilter(sub.Filter, 0, &remaining); err != nil {
		return err
	}
	if sub.Debounce == nil {
		return nil
	}
	d := sub.Debounce
	if (d.KeyAttribute == "") == (d.ConstantKey == "") {
		return errors.New("eventing: debounce requires exactly one key source")
	}
	if d.KeyAttribute != "" && !supportedAttribute(d.KeyAttribute) {
		return errors.New("eventing: unsupported debounce attribute")
	}
	if d.ConstantKey != "" && !boundedText(d.ConstantKey, 256) {
		return errors.New("eventing: invalid constant debounce key")
	}
	test := sub.Target
	test.Debounce = &Debounce{Key: "validation", Window: d.Window, MaxWait: d.MaxWait, MaxEvents: d.MaxEvents, InputMode: d.InputMode}
	return test.validate()
}

func validateFilter(f Filter, depth int, remaining *int) error {
	*remaining -= 1
	if depth > 8 || *remaining < 0 {
		return errors.New("eventing: subscription filter exceeds bound")
	}
	forms := 0
	if f.Attribute != "" || f.Equals != "" {
		forms++
	}
	if f.All != nil {
		forms++
	}
	if f.Any != nil {
		forms++
	}
	if f.Not != nil {
		forms++
	}
	if forms != 1 {
		return errors.New("eventing: filter requires exactly one expression")
	}
	if f.Attribute != "" || f.Equals != "" {
		if !supportedAttribute(f.Attribute) || !boundedText(f.Equals, 1024) {
			return errors.New("eventing: unsupported attribute equality")
		}
		return nil
	}
	if f.Not != nil {
		return validateFilter(*f.Not, depth+1, remaining)
	}
	children := f.All
	if f.Any != nil {
		children = f.Any
	}
	if len(children) < 1 || len(children) > 32 {
		return errors.New("eventing: filter group must contain 1..32 expressions")
	}
	for _, child := range children {
		if err := validateFilter(child, depth+1, remaining); err != nil {
			return err
		}
	}
	return nil
}

func supportedAttribute(attribute string) bool {
	switch attribute {
	case "id", "source", "type", "subject":
		return true
	default:
		return false
	}
}

// Match parses one envelope and derives a pinned plan inside the catalog gaggle.
// A missing debounce key fails that matched delivery, while unrelated consumers
// and no-match publications preserve their normal outcomes.
func (c *Catalog) Match(gaggle string, raw []byte) (Envelope, Plan, error) {
	if c == nil || gaggle != c.gaggle {
		return Envelope{}, Plan{}, errors.New("eventing: catalog gaggle differs")
	}
	envelope, err := Parse(raw)
	if err != nil {
		return Envelope{}, Plan{}, err
	}
	plan := Plan{Revision: c.revision, Routes: []Route{}}
	for _, sub := range c.subscriptions {
		if !matchFilter(sub.Filter, envelope) {
			continue
		}
		if len(plan.Routes) >= MaxConsumers {
			return Envelope{}, Plan{}, errors.New("eventing: matched fan-out exceeds limit")
		}
		route := sub.Target
		if sub.Debounce != nil {
			route = debounceRoute(route, *sub.Debounce, envelope)
		}
		plan.Routes = append(plan.Routes, route)
	}
	if _, err = plan.Marshal(); err != nil {
		return Envelope{}, Plan{}, err
	}
	return envelope, plan, nil
}

func matchFilter(f Filter, e Envelope) bool {
	if f.Attribute != "" {
		value, ok := envelopeAttribute(e, f.Attribute)
		return ok && value == f.Equals
	}
	if f.Not != nil {
		return !matchFilter(*f.Not, e)
	}
	if f.All != nil {
		for _, child := range f.All {
			if !matchFilter(child, e) {
				return false
			}
		}
		return true
	}
	for _, child := range f.Any {
		if matchFilter(child, e) {
			return true
		}
	}
	return false
}

func envelopeAttribute(e Envelope, attribute string) (string, bool) {
	switch attribute {
	case "id":
		return e.ID, true
	case "source":
		return e.Source, true
	case "type":
		return e.Type, true
	case "subject":
		return e.Subject, e.Subject != ""
	default:
		return "", false
	}
}

func debounceRoute(route Route, d DebouncePolicy, e Envelope) Route {
	value, kind := d.ConstantKey, "constant"
	if d.KeyAttribute != "" {
		var ok bool
		value, ok = envelopeAttribute(e, d.KeyAttribute)
		if !ok {
			route.FailureReason = "required debounce attribute is missing"
			return route
		}
		kind = "attribute:" + d.KeyAttribute
	}
	// Hash a framed key so long URI attributes are bounded without collisions
	// from delimiter concatenation, truncation or an implicit empty-key bucket.
	raw, _ := json.Marshal([]string{kind, value})
	route.Debounce = &Debounce{Key: fmt.Sprintf("sha256:%x", sha256.Sum256(raw)), Window: d.Window, MaxWait: d.MaxWait, MaxEvents: d.MaxEvents, InputMode: d.InputMode}
	return route
}
