package runtimewiring

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

type listerFake struct {
	items []providers.WorkItem
	err   error
	reqs  []providers.ListWorkItemsRequest
}

func (l *listerFake) ListWorkItems(_ context.Context, r providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
	l.reqs = append(l.reqs, r)
	return l.items, l.err
}

func intakeCfg(mode decisiongate.Mode, on bool) *instance.Config {
	return &instance.Config{DecisionGate: &decisiongate.Settings{
		Mode: mode, IntakeDetection: on, BaseURLEnv: "INTAKE_TEST_URL", KeyEnv: "INTAKE_TEST_KEY",
		ModelEnv: "INTAKE_TEST_MODEL", Fallback: decisiongate.FallbackAgent,
	}}
}

func TestIntakeShadowOptIn(t *testing.T) {
	t.Setenv("INTAKE_TEST_URL", "http://127.0.0.1:1")
	t.Setenv("INTAKE_TEST_KEY", "k")
	t.Setenv("INTAKE_TEST_MODEL", "m")
	claimed := []providers.WorkItem{{ID: "9", Title: "Cache eviction bug"}}
	open := []providers.WorkItem{{ID: "4", Title: "cache eviction bug"}}
	repo := providers.RepositoryRef{}

	for name, cfg := range map[string]*instance.Config{
		"nil":     nil,
		"off":     intakeCfg(decisiongate.ModeOff, true),
		"no flag": intakeCfg(decisiongate.ModeShadow, false),
		"enforce": intakeCfg(decisiongate.ModeEnforce, true),
		"no gate": {},
	} {
		l := &listerFake{items: open}
		if got := IntakeShadow(context.Background(), cfg, l, repo, claimed, nil); got != nil || len(l.reqs) != 0 {
			t.Fatalf("%s: expected inert, got %v (lists=%d)", name, got, len(l.reqs))
		}
	}

	l := &listerFake{items: open}
	got := IntakeShadow(context.Background(), intakeCfg(decisiongate.ModeShadow, true), l, repo, claimed, nil)
	if len(got) != 1 || got[0].Kind != decisiongate.IntakeDuplicate || got[0].Related != "4" {
		t.Fatalf("got %+v", got)
	}
	if len(l.reqs) != 1 || l.reqs[0].State != "open" || l.reqs[0].Limit != decisiongate.MaxIntakePool {
		t.Fatalf("retrieval not bounded to open items: %+v", l.reqs)
	}
}

func TestIntakeShadowRetrievalFailureIsInert(t *testing.T) {
	t.Setenv("INTAKE_TEST_URL", "http://127.0.0.1:1")
	t.Setenv("INTAKE_TEST_KEY", "k")
	t.Setenv("INTAKE_TEST_MODEL", "m")
	l := &listerFake{err: errors.New("api down")}
	got := IntakeShadow(context.Background(), intakeCfg(decisiongate.ModeShadow, true), l, providers.RepositoryRef{}, []providers.WorkItem{{ID: "1", Title: "t"}}, nil)
	if got != nil {
		t.Fatalf("got %v", got)
	}
}
