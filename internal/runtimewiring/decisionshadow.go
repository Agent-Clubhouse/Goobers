package runtimewiring

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
)

// podDecisionShadowWait bounds how long a pod stage waits for in-flight shadow
// scoring before surrendering; scoring is advisory and never holds a stage.
const podDecisionShadowWait = 15 * time.Second

// DecisionShadowObserver returns the advisory harness observer shared by the
// local and pod paths, so both emit the same decisiongate.shadow fields, and a
// wait function that blocks until scoring already started has finished.
// Successes are scored too so the log can show false alarms on healthy
// replies, not only detections on failed ones.
func DecisionShadowObserver(gate *decisiongate.Gate, sample float64, log *slog.Logger) (harness.Observer, func()) {
	newObs := func(outcome string) *decisiongate.Observer {
		return decisiongate.NewObserver(gate, sample, 2, func(r decisiongate.ShadowRecord) {
			errText := ""
			if r.Err != nil {
				errText = r.Err.Error()
			}
			log.Info("decisiongate.shadow",
				"outcome", outcome,
				"inputValid", ShadowInputValidity(r),
				"verdict", string(r.Verdict), "probability", r.Probability, "confidence", r.Confidence,
				"cached", r.Cached, "agentClaimedBad", r.AgentClaimedBad, "error", errText)
		})
	}
	ok, other := newObs("success"), newObs("non-success")
	observer := func(env apiv1.InvocationEnvelope, result apiv1.ResultEnvelope) {
		target := other
		if result.Status == apiv1.ResultSuccess {
			target = ok
		}
		if inputValid, known := DecisionShadowInputValidity(result); known {
			target.ObserveValidated(env.RunID, inputValid, result.Summary)
			return
		}
		target.Observe(env.RunID, result.Summary)
	}
	return observer, func() { ok.Wait(); other.Wait() }
}

// DecisionShadowInputValidity reads the deterministic handoff verdict a stage
// result carries, if any.
func DecisionShadowInputValidity(result apiv1.ResultEnvelope) (bool, bool) {
	report, ok := handoffcheck.ReportFromOutputs(result.Outputs)
	if !ok {
		return false, false
	}
	return report.Bool()
}

// ShadowInputValidity renders a record's input validity for the shadow log.
func ShadowInputValidity(record decisiongate.ShadowRecord) string {
	if !record.InputKnown {
		return string(handoffcheck.InputValidUnknown)
	}
	if record.InputValid {
		return string(handoffcheck.InputValidTrue)
	}
	return string(handoffcheck.InputValidFalse)
}

// PodDecisionShadowObserver builds the shadow observer for an agentic stage
// pod from the plane's delivery. It returns nil unless the delivery opts in to
// shadow mode and resolves to a usable gate: shadow mode must never stop a
// stage. The delivered key is registered with the stage's secret registry and
// every log line is scrubbed before it reaches stderr.
func PodDecisionShadowObserver(delivery *dispatcher.DecisionShadow, registry *journal.RegistryScrubber, stderr io.Writer) harness.Observer {
	if delivery == nil || delivery.Settings.EffectiveMode() != decisiongate.ModeShadow {
		return nil
	}
	if registry != nil {
		for _, value := range []string{delivery.APIKey, delivery.BaseURL} {
			if strings.TrimSpace(value) != "" {
				registry.Register([]byte(value))
			}
		}
	}
	var scrubber journal.Scrubber = journal.NewPatternScrubber()
	if registry != nil {
		scrubber = journal.Chain(registry, scrubber)
	}
	log := slog.New(slog.NewTextHandler(&scrubbedWriter{w: stderr, scrubber: scrubber}, nil))
	gate, err := delivery.Settings.Resolve(func(name string) string {
		switch name {
		case delivery.Settings.BaseURLEnv:
			return delivery.BaseURL
		case delivery.Settings.KeyEnv:
			return delivery.APIKey
		case delivery.Settings.ModelEnv:
			return delivery.Model
		}
		return ""
	}, nil)
	if err != nil {
		log.Warn("decisionGate disabled", "error", err.Error())
		return nil
	}
	observer, wait := DecisionShadowObserver(gate, delivery.Settings.ShadowSample, log)
	return func(env apiv1.InvocationEnvelope, result apiv1.ResultEnvelope) {
		observer(env, result)
		done := make(chan struct{})
		go func() { wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(podDecisionShadowWait):
		}
	}
}

type scrubbedWriter struct {
	mu       sync.Mutex
	w        io.Writer
	scrubber journal.Scrubber
}

func (s *scrubbedWriter) Write(p []byte) (int, error) {
	if s.w == nil {
		return len(p), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := io.Copy(s.w, bytes.NewReader(s.scrubber.Scrub(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
