//go:build cloudintegration

package childengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childadmissionclient"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
)

type qualificationWorkerConfig struct{ Address, Queue, Endpoint, KeyFile, InstanceID string }

// TestChildQualificationWorkerProcess is the subprocess entry point. In the
// normal suite it passes without starting anything; only StartWorker sets the
// private configuration path. The worker has HTTP and Temporal clients only.
func TestChildQualificationWorkerProcess(t *testing.T) {
	file := os.Getenv("GOOBERS_CHILD_QUALIFICATION_WORKER")
	if file == "" {
		return
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var cfg qualificationWorkerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	key, err := podauth.LoadKeyRingFile(cfg.KeyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.Dial(client.Options{HostPort: cfg.Address})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	registry := journal.NewRegistryScrubber()
	inner := childadmissionclient.AuthorityForWorker(cfg.Endpoint, cfg.Endpoint, "qualification-worker", key, registry)
	probeBlobs := &dispatcher.BlobClient{BaseURL: cfg.Endpoint, TokenSource: func() (string, error) { return key.MintWorkerBlob("qualification-worker", time.Minute) }}
	digest, canary := childresult.BlobPlaneCanary(cfg.InstanceID)
	authority := &childadmissionclient.ProbedAuthority{Inner: inner, Probe: func(ctx context.Context) error {
		got, err := probeBlobs.Get(ctx, digest)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, canary) {
			return errors.New("canary differs")
		}
		return nil
	}, Report: func(err error) { t.Logf("child authority withheld: %v", err) }}
	stages := qualificationStages{cfg: cfg, key: key, authority: authority}
	w := worker.New(c, cfg.Queue, worker.Options{WorkerStopTimeout: time.Second})
	engine.RegisterWith(w, &engine.Activities{EngineChildren: childadmissionclient.ForWorker(cfg.Endpoint, "qualification-worker", key),
		Journal: &livejournal.HTTPEmitter{BaseURL: cfg.Endpoint, Minter: key}})
	// The sole fixture boundary replaces an LLM/container executor. Everything
	// before and after these two stage activities is the production engine.
	for _, name := range []string{engine.ActInvokeGoober, "InvokeGooberCancelSafe"} {
		w.RegisterActivityWithOptions(stages.parent, activity.RegisterOptions{Name: name, DisableAlreadyRegisteredCheck: true})
	}
	for _, name := range []string{engine.ActRunDeterministic, "RunDeterministicCancelSafe"} {
		w.RegisterActivityWithOptions(stages.child, activity.RegisterOptions{Name: name, DisableAlreadyRegisteredCheck: true})
	}
	if err := w.Run(worker.InterruptCh()); err != nil {
		t.Fatal(err)
	}
}

type qualificationStages struct {
	cfg       qualificationWorkerConfig
	key       *podauth.SignedKey
	authority engine.ChildWorkflowAuthority
}

func (s qualificationStages) parent(ctx context.Context, env apiv1.InvocationEnvelope, _ string, _ string, _ apiv1.WorkspaceMode, _ string) (engine.DispatchStageResult, error) {
	for _, pointer := range env.ContextPointers {
		if !childresult.IsContinuationPointer(pointer) {
			continue
		}
		// Use a stage POD bearer, not the worker's credentials or control's Azure
		// handle. This is precisely the read a resumed parent needs to succeed.
		token, err := s.key.MintScoped(env.RunID, time.Minute, podauth.ScopeBlob)
		if err != nil {
			return engine.DispatchStageResult{}, err
		}
		blobs := &dispatcher.BlobClient{BaseURL: s.cfg.Endpoint, Token: token}
		receipt, err := blobs.Get(ctx, pointer.Artifact.Digest)
		if err != nil {
			return engine.DispatchStageResult{}, err
		}
		if journal.Digest(receipt) != pointer.Artifact.Digest || int64(len(receipt)) != pointer.Artifact.Size {
			return engine.DispatchStageResult{}, errors.New("child receipt bytes differ from continuation")
		}
		// Require the child's actual output, not just a successful blob GET.
		var sealed struct {
			Input childresult.TerminalResultInput `json:"input"`
			RunID string                          `json:"runId"`
		}
		if err := json.Unmarshal(receipt, &sealed); err != nil || sealed.Input.Summary != "qualification child result" || sealed.Input.State != "completed" || sealed.RunID == env.RunID || sealed.RunID == "" {
			return engine.DispatchStageResult{}, errors.New("child receipt lacks expected result")
		}
		return engine.DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "parent read child receipt through blob plane"}}, nil
	}
	if env.ChildWorkflowOrigin == nil {
		return engine.DispatchStageResult{}, errors.New("parent has no committed child origin")
	}
	grant, err := s.authority.AcquireChildAuthority(ctx, engine.ChildAuthorityRequest{RunID: env.RunID, Origin: *env.ChildWorkflowOrigin})
	if err != nil {
		return engine.DispatchStageResult{}, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.authority.ReleaseChildAuthority(cleanup, engine.ChildAuthorityRelease{RunID: env.RunID, Origin: *env.ChildWorkflowOrigin, GrantID: grant.GrantID})
	}()
	body, err := json.Marshal(map[string]string{"source": generatedChild})
	if err != nil {
		return engine.DispatchStageResult{}, err
	}
	path := strings.Replace(apicontract.ChildWorkflowStartPath, "{run}", env.RunID, 1)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return engine.DispatchStageResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+grant.BearerToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(httpapi.HeaderIdempotencyKey, "qualification-child")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return engine.DispatchStageResult{}, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return engine.DispatchStageResult{}, err
	}
	if response.StatusCode != http.StatusAccepted {
		return engine.DispatchStageResult{}, fmt.Errorf("child start HTTP %d: %s", response.StatusCode, raw)
	}
	var started apicontract.ChildWorkflowResponse
	if err := json.Unmarshal(raw, &started); err != nil {
		return engine.DispatchStageResult{}, err
	}
	handoff := journal.ChildHandoffRequest{Gaggle: env.Gaggle, ParentRunID: env.RunID, RequestID: journal.Digest([]byte(started.RunID)), Action: "wait",
		ChildRunID: started.RunID, AcceptanceID: started.AcceptanceID, InvocationKey: "qualification-child", SourceDigest: started.SourceDigest, Origin: *env.ChildWorkflowOrigin}
	return engine.DispatchStageResult{ChildWait: &handoff}, nil
}

func (s qualificationStages) child(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun, string, string) (engine.DispatchStageResult, error) {
	return engine.DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "qualification child result"}}, nil
}
