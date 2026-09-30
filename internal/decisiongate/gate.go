// Package decisiongate turns a decider.Decider into something a workflow can
// safely act on: per-question thresholds, a mandatory uncertain outcome,
// digest-keyed caching, a concurrency limit, and an observer hook for
// telemetry. Deterministic checks (schema, digest, status) belong in code and
// run before anything here.
package decisiongate

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/decider"
)

// Decision is the thresholded result of a noul question.
type Decision string

// Decisions. Uncertain is never collapsed into Yes or No: callers must choose
// a fallback for it.
const (
	Yes       Decision = "yes"
	No        Decision = "no"
	Uncertain Decision = "uncertain"
)

// Threshold maps a yes-probability to a Decision. p >= Accept is Yes,
// p <= Reject is No, anything between is Uncertain.
type Threshold struct {
	Accept float64 `json:"accept" yaml:"accept"`
	Reject float64 `json:"reject" yaml:"reject"`
}

func (t Threshold) validate() error {
	if t.Reject < 0 || t.Accept > 1 || t.Reject >= t.Accept {
		return fmt.Errorf("threshold needs 0 <= reject < accept <= 1, got reject=%v accept=%v", t.Reject, t.Accept)
	}
	return nil
}

// Config holds the tunables. Thresholds are keyed by question name so they can
// live in instance configuration rather than code.
type Config struct {
	Thresholds map[string]Threshold `json:"thresholds" yaml:"thresholds"`
	// MinConfidence, when above zero, downgrades choice answers below it to
	// Uncertain.
	MinConfidence float64 `json:"minConfidence" yaml:"minConfidence"`
	// MaxConcurrent bounds in-flight decider calls. Zero means 4.
	MaxConcurrent int `json:"maxConcurrent" yaml:"maxConcurrent"`
	// CacheEntries bounds the digest cache. Zero disables caching.
	CacheEntries int `json:"cacheEntries" yaml:"cacheEntries"`
	// CacheTTL expires cached answers. Zero means 1h.
	CacheTTL time.Duration `json:"cacheTTL" yaml:"cacheTTL"`
	// CallTimeout bounds one decider call. Zero means 10s.
	CallTimeout time.Duration `json:"callTimeout" yaml:"callTimeout"`
}

// Validate reports a configuration that cannot be used.
func (c Config) Validate() error {
	for name, t := range c.Thresholds {
		if err := t.validate(); err != nil {
			return fmt.Errorf("threshold %q: %w", name, err)
		}
	}
	if c.MinConfidence < 0 || c.MinConfidence > 1 {
		return errors.New("minConfidence must be within 0..1")
	}
	if c.MaxConcurrent < 0 || c.CacheEntries < 0 {
		return errors.New("maxConcurrent and cacheEntries must not be negative")
	}
	return nil
}

// Outcome is one judged question.
type Outcome struct {
	Name        string
	Decision    Decision
	Probability float64 // yes-probability for noul
	Choice      string  // top option for choice
	Confidence  float64
	Cached      bool
}

// Event is what the observer sees for every Judge call. It never carries the
// state itself, only its digest.
type Event struct {
	Name        string
	StateDigest string
	Outcome     Outcome
	Err         error
	Latency     time.Duration
}

// Gate applies Config to a Decider. Safe for concurrent use.
type Gate struct {
	d       decider.Decider
	cfg     Config
	sem     chan struct{}
	observe func(Event)
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]*list.Element
	order *list.List
}

type cacheEntry struct {
	key     string
	outcome Outcome
	expires time.Time
}

// New builds a Gate. observe may be nil.
func New(d decider.Decider, cfg Config, observe func(Event)) (*Gate, error) {
	if d == nil {
		return nil, errors.New("decisiongate: decider is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = time.Hour
	}
	if cfg.CallTimeout == 0 {
		cfg.CallTimeout = 10 * time.Second
	}
	return &Gate{
		d: d, cfg: cfg, observe: observe, now: time.Now,
		sem:   make(chan struct{}, cfg.MaxConcurrent),
		cache: map[string]*list.Element{}, order: list.New(),
	}, nil
}

// JudgeNoul asks a yes/no question and thresholds the answer. On any error the
// outcome is Uncertain and the error is returned, so a caller that ignores the
// error still gets the safe value.
func (g *Gate) JudgeNoul(ctx context.Context, name string, state any, q decider.Question) (Outcome, error) {
	start := g.now()
	th, ok := g.cfg.Thresholds[name]
	if !ok {
		return g.finish(name, "", Outcome{Name: name, Decision: Uncertain}, fmt.Errorf("decisiongate: no threshold configured for %q", name), start)
	}
	if q.Type != decider.KindNoul {
		return g.finish(name, "", Outcome{Name: name, Decision: Uncertain}, fmt.Errorf("decisiongate: %q is not a noul question", name), start)
	}
	digest, err := digestOf(name, state, q)
	if err != nil {
		return g.finish(name, "", Outcome{Name: name, Decision: Uncertain}, err, start)
	}
	if o, hit := g.lookup(digest); hit {
		o.Cached = true
		return g.finish(name, digest, o, nil, start)
	}

	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	case <-ctx.Done():
		return g.finish(name, digest, Outcome{Name: name, Decision: Uncertain}, ctx.Err(), start)
	}
	cctx, cancel := context.WithTimeout(ctx, g.cfg.CallTimeout)
	defer cancel()
	resp, err := g.d.Decide(cctx, decider.Request{State: state, Questions: map[string]decider.Question{name: q}})
	if err != nil {
		return g.finish(name, digest, Outcome{Name: name, Decision: Uncertain}, err, start)
	}
	ans, ok := resp.Answers[name]
	if !ok || ans.Yes == nil {
		return g.finish(name, digest, Outcome{Name: name, Decision: Uncertain}, fmt.Errorf("decisiongate: no noul answer for %q", name), start)
	}
	o := Outcome{Name: name, Probability: *ans.Yes, Decision: Uncertain}
	switch {
	case *ans.Yes >= th.Accept:
		o.Decision = Yes
	case *ans.Yes <= th.Reject:
		o.Decision = No
	}
	g.store(digest, o)
	return g.finish(name, digest, o, nil, start)
}

func (g *Gate) finish(name, digest string, o Outcome, err error, start time.Time) (Outcome, error) {
	if g.observe != nil {
		g.observe(Event{Name: name, StateDigest: digest, Outcome: o, Err: err, Latency: g.now().Sub(start)})
	}
	return o, err
}

func digestOf(name string, state any, q decider.Question) (string, error) {
	b, err := json.Marshal(struct {
		N string
		S any
		I any
		T decider.Kind
	}{name, state, q.Instructions, q.Type})
	if err != nil {
		return "", fmt.Errorf("decisiongate: state is not JSON-encodable: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (g *Gate) lookup(key string) (Outcome, bool) {
	if g.cfg.CacheEntries == 0 {
		return Outcome{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	el, ok := g.cache[key]
	if !ok {
		return Outcome{}, false
	}
	e := el.Value.(*cacheEntry)
	if g.now().After(e.expires) {
		g.order.Remove(el)
		delete(g.cache, key)
		return Outcome{}, false
	}
	g.order.MoveToFront(el)
	return e.outcome, true
}

func (g *Gate) store(key string, o Outcome) {
	if g.cfg.CacheEntries == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if el, ok := g.cache[key]; ok {
		g.order.Remove(el)
	}
	g.cache[key] = g.order.PushFront(&cacheEntry{key: key, outcome: o, expires: g.now().Add(g.cfg.CacheTTL)})
	for g.order.Len() > g.cfg.CacheEntries {
		last := g.order.Back()
		g.order.Remove(last)
		delete(g.cache, last.Value.(*cacheEntry).key)
	}
}
