package livejournal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/goobers/goobers/internal/planehttp"
)

// ChildOriginActivity names the actual resident activity loan. Control verifies
// these coordinates and the entire batch against Temporal, never caller claims.
type ChildOriginActivity struct {
	WorkflowRunID string `json:"workflowRunId"`
	ActivityID    string `json:"activityId"`
	Attempt       int32  `json:"attempt"`
}

// ChildOriginEmitRequest carries the complete recorded batch and its activity loan.
type ChildOriginEmitRequest struct {
	Activity ChildOriginActivity `json:"activity"`
	Batch    EmitRequest         `json:"batch"`
}

type childOriginActivityKey struct{}

// WithChildOriginActivity attaches trusted SDK activity coordinates for emission.
func WithChildOriginActivity(ctx context.Context, activity ChildOriginActivity) context.Context {
	return context.WithValue(ctx, childOriginActivityKey{}, activity)
}

// ChildOriginEmitter verifies a resident worker loan before committing origins.
type ChildOriginEmitter interface {
	EmitChildOrigin(context.Context, string, ChildOriginEmitRequest) (EmitResponse, error)
}

type childOriginMinter interface {
	MintWorkerChildAdmission(string, time.Duration) (string, error)
}

// HasChildStageStart selects the confined host seam; ordinary pod emission
// remains unchanged. The controller derives origins for the recorded batch.
func (r EmitRequest) HasChildStageStart() bool {
	for _, op := range r.Ops {
		if op.ChildStageStart != nil {
			return true
		}
	}
	return false
}

func (e *HTTPEmitter) emitChildOrigin(ctx context.Context, batch EmitRequest) (EmitResponse, error) {
	proof, ok := ctx.Value(childOriginActivityKey{}).(ChildOriginActivity)
	minter, mintable := e.Minter.(childOriginMinter)
	if !ok || !mintable || proof.WorkflowRunID == "" || proof.ActivityID == "" || proof.Attempt < 1 || e.ChildOriginWorkerID == "" {
		return EmitResponse{}, errors.New("livejournal: child origin requires a resident worker activity")
	}
	token, err := minter.MintWorkerChildAdmission(e.ChildOriginWorkerID, mintedTokenTTL)
	if err != nil {
		return EmitResponse{}, err
	}
	client := http.Client{Timeout: emitHTTPTimeout}
	if e.Client != nil {
		client = *e.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout <= 0 || client.Timeout > emitHTTPTimeout {
		client.Timeout = emitHTTPTimeout
	}
	transport, err := planehttp.New(planehttp.Config{BaseURL: e.BaseURL, Token: token, Client: &client})
	if err != nil {
		return EmitResponse{}, err
	}
	response, err := transport.DoJSON(ctx, http.MethodPost, "/api/v1/engine/children/origin", ChildOriginEmitRequest{Activity: proof, Batch: batch}, nil)
	if err != nil {
		return EmitResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return EmitResponse{}, fmt.Errorf("livejournal: worker child-origin emit refused (HTTP %d)", response.StatusCode)
	}
	data, err := planehttp.ReadBounded(response.Body, 4<<20)
	if err != nil {
		return EmitResponse{}, err
	}
	var out EmitResponse
	err = json.Unmarshal(data, &out)
	return out, err
}
