package stateclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
)

// The pod-scoped priority trigger (#3878, decision 005 ruling R3's second
// half: "pod principals may POST triggers only for their own gaggle").
//
// apply-verdict's crowned-lander re-tick publishes a blocker record and then
// asks the scheduler to re-select immediately instead of waiting for the next
// poll. Locally that is a request file dropped into the instance's
// pending-triggers directory, which the daemon's sweep picks up. A stage pod
// has no such directory — the scheduler directory it can see is its own
// container's, and nothing sweeps it — so the re-tick is silently lost.
//
// The trigger plane is the same daemon API the scheduler-state route is served
// from, reached with the same bearer: the pod principal that is allowed to
// compare-and-swap its gaggle's scheduler state is exactly the principal
// allowed to ask for its gaggle's re-tick, and the daemon verifies both
// against the same authority (the caller's own run.yaml). One selection, one
// credential, one containment rule.

// ErrPriorityTriggerUnavailable reports a priority trigger attempted against a
// backend that has no plane to reach — the file backend. Callers fall back to
// the local request-file drop, which is what a non-pod stage should be doing
// anyway.
var ErrPriorityTriggerUnavailable = errors.New("stateclient: priority triggers require the scheduler plane")

// maxTriggerBodyBytes bounds what is read back from the trigger route: the
// response is a run id and a flag, and an error envelope is small. Anything
// larger is a misrouted response, not something to buffer.
const maxTriggerBodyBytes = 64 << 10

// PriorityTriggerer is the plane-only half of the stage seam. Callers type-
// assert their Store for it: an assertion that fails means "no plane here",
// which is a routing fact, not an error.
type PriorityTriggerer interface {
	// PriorityTrigger asks the daemon to re-tick workflow in the client's
	// gaggle because sourceRun published new durable selection state. The
	// returned run id may be empty when the daemon admitted the trigger but
	// scheduling deferred the mint.
	PriorityTrigger(ctx context.Context, workflow, sourceRun string) (string, error)
}

// triggerRequest mirrors httpapi.TriggerRequest's wire form. It is duplicated
// rather than imported for the same reason the claims client duplicates its
// own: internal/httpapi is the SERVER, and a stage binary linking the server's
// package would drag the whole daemon surface into every stage image.
type triggerRequest struct {
	Gaggle    string `json:"gaggle,omitempty"`
	Workflow  string `json:"workflow"`
	RequestID string `json:"requestId,omitempty"`
	SourceRun string `json:"sourceRun,omitempty"`
}

type triggerResponse struct {
	RunID     string `json:"runId,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

// PriorityTrigger implements PriorityTriggerer.
func (h *HTTP) PriorityTrigger(ctx context.Context, workflow, sourceRun string) (string, error) {
	if workflow == "" || sourceRun == "" {
		return "", fmt.Errorf("stateclient: priority trigger requires a workflow and a source run")
	}
	// A retried activity must redeliver the same trigger rather than mint a
	// second run after an ambiguous transport failure.
	requestID := "priority-" + ETagFor([]byte(h.cfg.Gaggle+"\x00"+workflow+"\x00"+sourceRun))
	// The gaggle is the client's own, never the caller's to choose: the daemon
	// refuses a gaggle the caller's run does not belong to, and sending
	// anything else would only turn a working re-tick into a 403.
	body, err := json.Marshal(triggerRequest{
		Gaggle: h.cfg.Gaggle, Workflow: workflow, RequestID: requestID, SourceRun: sourceRun,
	})
	if err != nil {
		return "", fmt.Errorf("stateclient: encode trigger: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.BaseURL+apicontract.TriggerIngestPath, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("stateclient: build trigger request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := h.cfg.Client.Do(request)
	if err != nil {
		return "", fmt.Errorf("stateclient: trigger: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxTriggerBodyBytes))
	if err != nil {
		return "", fmt.Errorf("stateclient: read trigger response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", planeError(response.StatusCode, raw)
	}
	var decoded triggerResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("stateclient: decode trigger response: %w", err)
	}
	return decoded.RunID, nil
}

// PriorityTrigger implements PriorityTriggerer for the file backend by
// refusing: there is no daemon on the other side of a file, and inventing a
// local mint here would bypass scheduler admission entirely.
func (f *File) PriorityTrigger(context.Context, string, string) (string, error) {
	return "", ErrPriorityTriggerUnavailable
}
