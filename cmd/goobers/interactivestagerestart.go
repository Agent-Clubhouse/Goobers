package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
)

type interactiveStageRestart struct {
	layout  instance.Layout
	setup   *schedulerSetup
	service *intervention.Service
}

func restartRefusal(code, message string) error {
	return httpapi.NewInterventionError(http.StatusConflict, code, message, nil)
}

func (s *interactiveStageRestart) RestartStage(admission, execution context.Context, principal httpapi.Principal, plan runner.StageRestartPlan) (intervention.StageRestartAcceptance, error) {
	if s == nil || s.setup == nil || s.setup.InteractiveAccess == nil || s.service == nil {
		return intervention.StageRestartAcceptance{}, restartRefusal("restart_unavailable", "Interactive restart admission is unavailable.")
	}
	// A policy reload cannot wait behind an unbounded provider preflight.
	ctx, cancel := context.WithTimeout(admission, 30*time.Second)
	defer cancel()
	var result intervention.StageRestartAcceptance
	err := s.setup.InteractiveAccess.WithRestartAdmission(ctx, principal, plan.Source.Gaggle, func(ctx context.Context, load interactiveaccess.RestartSourceLoader) error {
		if err := stampRestartAuthority(s.layout, principal, &plan); err != nil {
			return err
		}
		// The same per-run custody fence used for terminal child snapshots also
		// prevents a legacy intervention from changing the source during restart.
		release, exclusive := s.setup.RunnerRegistry.acquireChildCustody(plan.Source.RunID)
		if !exclusive {
			return restartRefusal("restart_source_active", "The source still has an execution or custody owner.")
		}
		defer release()
		var err error
		result, err = s.service.LaunchStageRestart(ctx, execution, plan, func(ctx context.Context, candidate *runner.StageRestartPlan) ([]localscheduler.ClaimEntry, error) {
			return s.preflight(ctx, candidate, load)
		})
		return err
	})
	return result, err
}

// A retry retains the original verified claim snapshot. Reauthentication can
// change role/group order or membership without changing the accepted command;
// current authorization was independently checked before entering this helper.
func stampRestartAuthority(layout instance.Layout, p httpapi.Principal, plan *runner.StageRestartPlan) error {
	a, err := interactiveaccess.NewRestartAuthority(p, plan.Source, plan.Continuation.RunID, plan.Continuation.Target, plan.Continuation.ExpectedTerminalSeq)
	if err != nil {
		return err
	}
	dir := filepath.Join(layout.ForGaggle(plan.Source.Gaggle).RunsDir(), plan.Continuation.RunID)
	if _, err := os.Stat(dir); err == nil {
		reader, err := journal.OpenReadOnly(dir)
		if err != nil {
			return err
		}
		id, err := reader.Identity()
		if err != nil {
			return err
		}
		retained, err := interactiveaccess.LoadRestartAuthority(reader, id)
		if err != nil {
			return err
		}
		if retained.Issuer != a.Issuer || retained.Subject != a.Subject || retained.SourceRunID != a.SourceRunID || retained.SourceTerminalSeq != a.SourceTerminalSeq || retained.Gaggle != a.Gaggle || retained.Stage != a.Stage || retained.WorkflowDigest != a.WorkflowDigest || retained.GooberDigest != a.GooberDigest {
			return restartRefusal("idempotency_key_reused", "This restart key belongs to a different source or human identity.")
		}
		a = retained
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := a.Marshal()
	if err != nil {
		return err
	}
	if plan.Continuation.Operator != a.Issuer+":"+a.Subject {
		return interactiveaccess.ErrDenied
	}
	if plan.Continuation.Inputs == nil {
		plan.Continuation.Inputs = map[string][]byte{}
	}
	if plan.Continuation.InputIntegrity == nil {
		plan.Continuation.InputIntegrity = map[string]apiv1.Integrity{}
	}
	if plan.Continuation.InputSource == nil {
		plan.Continuation.InputSource = map[string]string{}
	}
	plan.Continuation.Inputs[interactiveaccess.RestartAuthorityInputName] = raw
	plan.Continuation.InputIntegrity[interactiveaccess.RestartAuthorityInputName] = apiv1.IntegrityTrusted
	plan.Continuation.InputSource[interactiveaccess.RestartAuthorityInputName] = plan.Continuation.Operator
	return nil
}
