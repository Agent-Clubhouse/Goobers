package decisiongate

import (
	"context"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// claimPattern is the deterministic, model-free detector for "the agent says
// its input was bad". It is the shadow baseline the model is compared against.
var claimPattern = regexp.MustCompile(`(?i)\b(corrupt(ed)?|truncat(ed|ion)|malformed|unreadable|incomplete|invalid json|could not (read|parse)|cannot (read|parse))\b`)

// ClaimsBadInput reports whether text lexically claims bad input.
func ClaimsBadInput(text string) bool { return claimPattern.MatchString(text) }

// Observer scores stage replies in the background. It never blocks the caller:
// when all workers are busy the reply is dropped and counted.
type Observer struct {
	Gate    *Gate
	Sample  float64
	Timeout time.Duration
	Record  func(ShadowRecord)
	sem     chan struct{}
	dropped atomic.Int64
	wg      sync.WaitGroup
}

// NewObserver builds an Observer allowing at most workers concurrent scorings.
func NewObserver(g *Gate, sample float64, workers int, record func(ShadowRecord)) *Observer {
	if workers <= 0 {
		workers = 2
	}
	return &Observer{Gate: g, Sample: sample, Timeout: 8 * time.Second, Record: record, sem: make(chan struct{}, workers)}
}

// Dropped is the number of replies skipped because workers were busy.
func (o *Observer) Dropped() int64 { return o.dropped.Load() }

// Wait blocks until in-flight scorings finish (tests and shutdown).
func (o *Observer) Wait() { o.wg.Wait() }

// Observe schedules a shadow scoring of reply. sampleKey (the run id) makes
// sampling stable per run. Only non-empty replies are eligible.
func (o *Observer) Observe(sampleKey, reply string) {
	o.observe(sampleKey, reply, nil)
}

// ObserveValidated schedules a shadow scoring of reply against deterministic
// input-validity ground truth.
func (o *Observer) ObserveValidated(sampleKey string, inputValid bool, reply string) {
	o.observe(sampleKey, reply, &inputValid)
}

func (o *Observer) observe(sampleKey, reply string, inputValid *bool) {
	if o == nil || o.Gate == nil || reply == "" || !Sampled(sampleKey, o.Sample) {
		return
	}
	select {
	case o.sem <- struct{}{}:
	default:
		o.dropped.Add(1)
		return
	}
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer func() { <-o.sem }()
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
		defer cancel()
		if inputValid != nil {
			o.Record(o.Gate.Shadow(ctx, *inputValid, ClaimsBadInput(reply), reply))
			return
		}
		o.Record(o.Gate.ShadowReply(ctx, ClaimsBadInput(reply), reply))
	}()
}
