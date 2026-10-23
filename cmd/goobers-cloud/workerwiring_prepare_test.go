package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
)

func TestWorkerGooberPreparationReleaseAndOrder(t *testing.T) {
	setupErr := errors.New("setup failed")
	operations := []struct {
		name string
		run  func(workerGoober, apiv1.InvocationEnvelope) (bool, error)
	}{
		{
			name: "Invoke",
			run: func(goober workerGoober, env apiv1.InvocationEnvelope) (bool, error) {
				result, err := goober.Invoke(t.Context(), env)
				return reflect.DeepEqual(result, apiv1.ResultEnvelope{}), err
			},
		},
		{
			name: "Review",
			run: func(goober workerGoober, env apiv1.InvocationEnvelope) (bool, error) {
				verdict, err := goober.Review(t.Context(), env)
				return reflect.DeepEqual(verdict, apiv1.Verdict{}), err
			},
		},
	}
	cases := []struct {
		name         string
		failure      string
		wantReleases int
		wantOrder    []string
	}{
		{name: "acquisition error", failure: "acquire"},
		{name: "harness error", failure: "harness", wantReleases: 1, wantOrder: []string{"release"}},
		{name: "executor error", failure: "executor", wantReleases: 1, wantOrder: []string{"release"}},
		{name: "materialization error", failure: "materialize", wantReleases: 1, wantOrder: []string{"materialize", "release"}},
		{name: "authority error", failure: "authority", wantReleases: 1, wantOrder: []string{"materialize", "authority", "release"}},
		{name: "success", wantReleases: 1, wantOrder: []string{"materialize", "authority", "dispatch", "release"}},
	}

	for _, operation := range operations {
		for _, tc := range cases {
			t.Run(operation.name+"/"+tc.name, func(t *testing.T) {
				var releases int
				var order []string
				exec := &preparationCaptureGoober{dispatch: func() { order = append(order, "dispatch") }}
				gaggle := &gaggleSeams{
					cfg: runner.Config{NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
						return exec, nil
					}},
					runsDir: t.TempDir(),
				}
				env := apiv1.InvocationEnvelope{
					RunID: "run", TaskID: "run:task", Gaggle: "example", WorkflowID: "workflow", Goober: "coder",
				}
				if tc.failure == "harness" {
					gaggle.harnessRefusals = map[localscheduler.WorkflowIdentity]string{
						{Gaggle: env.Gaggle, Workflow: env.WorkflowID}: "unavailable",
					}
				}
				if tc.failure == "executor" {
					env.Goober = ""
				}
				adapter := workerGoober{
					seams: &workerSeams{},
					acquire: func(context.Context, apiv1.InvocationEnvelope) (*gaggleSeams, func(), error) {
						if tc.failure == "acquire" {
							return nil, nil, setupErr
						}
						return gaggle, func() {
							releases++
							order = append(order, "release")
						}, nil
					},
					materialize: func(context.Context, *gaggleSeams, apiv1.InvocationEnvelope) error {
						order = append(order, "materialize")
						if tc.failure == "materialize" {
							return setupErr
						}
						return nil
					},
					mergeAuthority: func(ctx context.Context, _ apiv1.InvocationEnvelope) (context.Context, error) {
						order = append(order, "authority")
						if tc.failure == "authority" {
							return nil, setupErr
						}
						return ctx, nil
					},
				}

				zero, err := operation.run(adapter, env)
				if tc.failure == "" {
					if err != nil {
						t.Fatalf("operation failed: %v", err)
					}
					if zero {
						t.Fatal("successful operation returned its zero value")
					}
				} else {
					if err == nil {
						t.Fatal("operation succeeded on a setup error")
					}
					if !zero {
						t.Fatal("setup error returned a non-zero value")
					}
				}
				if releases != tc.wantReleases {
					t.Fatalf("release called %d times, want %d", releases, tc.wantReleases)
				}
				if !reflect.DeepEqual(order, tc.wantOrder) {
					t.Fatalf("operation order = %v, want %v", order, tc.wantOrder)
				}
			})
		}
	}
}

func TestWorkerGooberPreparationReleasesOnPanic(t *testing.T) {
	panicValue := errors.New("setup panic")
	operations := []struct {
		name string
		run  func(workerGoober, apiv1.InvocationEnvelope)
	}{
		{
			name: "Invoke",
			run: func(goober workerGoober, env apiv1.InvocationEnvelope) {
				_, _ = goober.Invoke(t.Context(), env)
			},
		},
		{
			name: "Review",
			run: func(goober workerGoober, env apiv1.InvocationEnvelope) {
				_, _ = goober.Review(t.Context(), env)
			},
		},
	}
	panicStages := []string{"executor", "materialize", "authority"}

	for _, operation := range operations {
		for _, panicStage := range panicStages {
			t.Run(operation.name+"/"+panicStage, func(t *testing.T) {
				var releases int
				gaggle := &gaggleSeams{
					cfg: runner.Config{NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
						if panicStage == "executor" {
							panic(panicValue)
						}
						return &preparationCaptureGoober{dispatch: func() {}}, nil
					}},
					runsDir: t.TempDir(),
				}
				env := apiv1.InvocationEnvelope{
					RunID: "run", TaskID: "run:task", Gaggle: "example", WorkflowID: "workflow", Goober: "coder",
				}
				adapter := workerGoober{
					seams: &workerSeams{},
					acquire: func(context.Context, apiv1.InvocationEnvelope) (*gaggleSeams, func(), error) {
						return gaggle, func() { releases++ }, nil
					},
					materialize: func(context.Context, *gaggleSeams, apiv1.InvocationEnvelope) error {
						if panicStage == "materialize" {
							panic(panicValue)
						}
						return nil
					},
					mergeAuthority: func(ctx context.Context, _ apiv1.InvocationEnvelope) (context.Context, error) {
						if panicStage == "authority" {
							panic(panicValue)
						}
						return ctx, nil
					},
				}

				func() {
					defer func() {
						recovered := recover()
						recoveredErr, ok := recovered.(error)
						if !ok || !errors.Is(recoveredErr, panicValue) {
							t.Fatalf("recovered %v, want %v", recovered, panicValue)
						}
					}()
					operation.run(adapter, env)
				}()
				if releases != 1 {
					t.Fatalf("release called %d times, want 1", releases)
				}
			})
		}
	}
}

type preparationCaptureGoober struct {
	dispatch func()
}

func (g *preparationCaptureGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.dispatch()
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (g *preparationCaptureGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	g.dispatch()
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}
