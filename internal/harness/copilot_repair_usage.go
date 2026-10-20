package harness

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/telemetry"
)

// Copilot repair creates a new session, unlike Claude/Codex resume. Keep one
// authoritative snapshot per session and sum sessions only after all fallback
// sources for each session have been resolved.
type copilotRepairUsage struct {
	transcriptPath, usagePath string
	previousTranscript        string
	previous                  *Outcome
	checkpoints               *copilotTranscriptCheckpoints
}

func (u *copilotRepairUsage) prepare(ctx context.Context, runner ProcessRunner, req RunRequest, argv, env []string) ([]string, []string, error) {
	if u.transcriptPath == "" {
		if _, controlled := runner.(*copilotControlledRunner); !controlled {
			// Caller-managed --resume/--connect sessions have no owned native
			// capture and may report cumulative totals. Keep their existing
			// final-snapshot semantics rather than assuming a fresh session.
			return argv, env, nil
		}
	}
	argv = append([]string(nil), argv...)
	nextTranscript, nextUsage := u.transcriptPath, u.usagePath
	if u.transcriptPath != "" {
		if home, ok := copilotConfigHome(env); ok && filepath.Base(u.transcriptPath) == "events.jsonl" {
			oldID := filepath.Base(filepath.Dir(u.transcriptPath))
			id, err := newHarnessSessionID()
			if err != nil {
				return nil, nil, fmt.Errorf("create repair session id: %w", err)
			}
			// Covers both the ordinary --session-id and launcher templates.
			for i, arg := range argv {
				argv[i] = strings.ReplaceAll(arg, oldID, id)
			}
			nextTranscript = copilotSessionLogPath(home, id)
		} else {
			// Wrapper-managed sessions export into the adapter's private directory.
			nextTranscript = filepath.Join(filepath.Dir(u.transcriptPath), "repair.jsonl")
			env = overrideEnv(env, "GOOBERS_SESSION_TRANSCRIPT", nextTranscript)
		}
	}
	if u.usagePath != "" {
		nextUsage = filepath.Join(filepath.Dir(u.usagePath), "repair-usage.json")
		for i, arg := range argv {
			if arg == "--usage-output-file" && i+1 < len(argv) {
				argv[i+1] = filepath.Join(filepath.Dir(argv[i+1]), "repair-usage.json")
			}
		}
	}

	// Shutdown publishes the controlled session's totals before Restart closes
	// its process. Capture errors remain visible, and the initial snapshot is
	// still retained when a later repair fails to open or reports no usage.
	finishErr := finalizeControlledCopilot(ctx, runner)
	previous := &Outcome{}
	if native, ok := readCopilotSessionTranscript(u.transcriptPath, req.MaxTranscriptBytes); ok {
		previous.Metrics, previous.ModelUsage = native.metrics, native.modelUsage
	}
	applyControlledCopilotUsage(previous, runner)
	applyCopilotUsageDocument(previous, u.usagePath)
	if finishErr != nil {
		return nil, nil, finishErr
	}
	if u.checkpoints != nil {
		if err := u.checkpoints.follow(req, nextTranscript, env); err != nil {
			return nil, nil, err
		}
	}
	u.previous = previous
	u.previousTranscript = u.transcriptPath
	u.transcriptPath, u.usagePath = nextTranscript, nextUsage
	if controlled, ok := runner.(*copilotControlledRunner); ok {
		controlled.usage = transcriptCapture{}
	}
	return argv, env, nil
}

func (u *copilotRepairUsage) apply(out *Outcome) {
	if u.previous == nil {
		return
	}
	if out.Metrics == nil {
		out.Metrics = make(map[string]float64)
	}
	for key, value := range u.previous.Metrics {
		out.Metrics[key] += value
	}
	models := make(map[string]telemetry.ModelUsage)
	for _, usage := range append(append([]telemetry.ModelUsage(nil), u.previous.ModelUsage...), out.ModelUsage...) {
		prior, exists := models[usage.Model]
		if !exists {
			models[usage.Model] = usage
			continue
		}
		prior.InputTokens = sumCopilotUsage(prior.InputTokens, usage.InputTokens)
		prior.OutputTokens = sumCopilotUsage(prior.OutputTokens, usage.OutputTokens)
		prior.CacheReadTokens = sumCopilotUsage(prior.CacheReadTokens, usage.CacheReadTokens)
		prior.CacheWriteTokens = sumCopilotUsage(prior.CacheWriteTokens, usage.CacheWriteTokens)
		prior.ReasoningTokens = sumCopilotUsage(prior.ReasoningTokens, usage.ReasoningTokens)
		prior.CopilotPremiumRequests = sumCopilotUsage(prior.CopilotPremiumRequests, usage.CopilotPremiumRequests)
		prior.NanoAIU = sumCopilotUsage(prior.NanoAIU, usage.NanoAIU)
		prior.CostUSD = sumCopilotUsage(prior.CostUSD, usage.CostUSD)
		if prior.CostBasis != usage.CostBasis {
			prior.CostBasis = telemetry.CostBasisUnknown
		}
		if prior.BillingModel != usage.BillingModel {
			prior.BillingModel = ""
		}
		models[usage.Model] = prior
	}
	out.ModelUsage = nil
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out.ModelUsage = append(out.ModelUsage, models[name])
	}
}

func sumCopilotUsage[T int64 | float64](first, second *T) *T {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	sum := *first + *second
	return &sum
}

func (u *copilotRepairUsage) applyTelemetry(out *Outcome, req RunRequest, runner ProcessRunner, emitter *adapterAgentEmitter) error {
	err := applyCopilotNativeTelemetry(out, req, u.transcriptPath, emitter, u.previousTranscript)
	applyControlledCopilotUsage(out, runner)
	applyCopilotUsageDocument(out, u.usagePath, nil)
	u.apply(out)
	if req.UsageSink != nil {
		req.UsageSink(maps.Clone(out.Metrics))
	}
	return err
}
