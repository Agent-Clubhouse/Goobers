//go:build cloudintegration

package childengine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childadmissionclient"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
)

// qualificationAge changes only scenario timestamps with a separate setup
// connection. Runtime pools retain their controller role and column grants.
func qualificationAge(h *qualificationHarness, query string, args ...any) {
	h.t.Helper()
	connection, err := pgx.ConnectConfig(h.ctx, h.pool.Config().ConnConfig)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = connection.Close(h.ctx) }()
	tag, err := connection.Exec(h.ctx, query, args...)
	if err != nil || tag.RowsAffected() != 1 {
		h.t.Fatalf("age one retained timestamp row: rows=%d (%v)", tag.RowsAffected(), err)
	}
}

// qualificationBoundRequest re-delivers the worker's already-bound grant
// through the production authority client. It cannot silently mint a new grant.
func qualificationBoundRequest(h *qualificationHarness) *qualificationSubmission {
	h.t.Helper()
	request := engine.ChildAuthorityRequest{RunID: h.input.RunID}
	for _, event := range qualificationEvents(h) {
		if event.Type != journal.EventStageStarted {
			continue
		}
		origin, err := journal.ChildWorkflowOriginForEvent(h.input.RunID, event)
		if err != nil {
			h.t.Fatal(err)
		}
		if origin != nil {
			request.Origin = *origin
		}
	}
	var boundID string
	if err := h.pool.QueryRow(h.ctx, `SELECT grant_id FROM control.child_authority
		WHERE instance_id=$1 AND parent_run_id=$2 AND occurrence=$3 AND NOT revoked`,
		testInstance, h.input.RunID, request.Origin.StageOccurrence).Scan(&boundID); err != nil {
		h.t.Fatal(err)
	}
	key, err := podauth.LoadKeyRingFile(h.keyFile, "")
	if err != nil {
		h.t.Fatal(err)
	}
	authority := childadmissionclient.AuthorityForWorker(h.server.URL, h.server.URL, "qualification-probe", key, journal.NewRegistryScrubber())
	grant, err := authority.AcquireChildAuthority(h.ctx, request)
	if err != nil || grant.GrantID != boundID {
		h.t.Fatalf("re-delivery did not preserve the worker's bound grant: %v", err)
	}
	body, err := json.Marshal(map[string]string{"source": generatedChild})
	if err != nil {
		h.t.Fatal(err)
	}
	return &qualificationSubmission{body: body, header: http.Header{
		"Authorization": {"Bearer " + grant.BearerToken}, "Content-Type": {"application/json"},
	}}
}

type qualificationSubmission struct {
	body   []byte
	header http.Header
}

func qualificationProbe(h *qualificationHarness, captured *qualificationSubmission, path, key string, status int, code string) []byte {
	h.t.Helper()
	path = strings.Replace(path, "{run}", h.input.RunID, 1)
	request, err := http.NewRequestWithContext(h.ctx, http.MethodPost, h.server.URL+path, bytes.NewReader(captured.body))
	if err != nil {
		h.t.Fatal(err)
	}
	request.Header = captured.header.Clone()
	if key != "" {
		request.Header.Set(httpapi.HeaderIdempotencyKey, key)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		h.t.Fatal(err)
	}
	if response.StatusCode != status {
		h.t.Fatalf("%s: HTTP %d, want %d", path, response.StatusCode, status)
	}
	if code != "" {
		var envelope apicontract.ErrorEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Error.Code != code {
			h.t.Fatalf("%s: refusal %q, want %q (%v)", path, envelope.Error.Code, code, err)
		}
	}
	return raw
}

func qualificationGrant(h *qualificationHarness, request *qualificationSubmission) podauth.ChildWorkflowGrant {
	h.t.Helper()
	key, err := podauth.LoadKeyRingFile(h.keyFile, podauth.CredentialGrantKeyDomain)
	if err != nil {
		h.t.Fatal(err)
	}
	grant, err := key.VerifyChildWorkflowGrant(strings.TrimPrefix(request.header.Get("Authorization"), "Bearer "))
	if err != nil {
		h.t.Fatal("verify captured grant:", err)
	}
	return grant
}

func qualificationEvents(h *qualificationHarness) []journal.Event {
	h.t.Helper()
	reader, err := h.epoch.backend.OpenRunReader(h.ctx, journal.RunKey{Gaggle: "example", RunID: h.input.RunID})
	if err != nil {
		h.t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		h.t.Fatal(err)
	}
	return events
}

func qualificationChildCompleted(h *qualificationHarness, run client.WorkflowRun) {
	h.t.Helper()
	var result engine.RunResult
	if err := run.Get(h.ctx, &result); err != nil {
		h.t.Fatal(err)
	}
	if result.Status != engine.StatusCompleted || result.Outputs["plan"].Summary != "parent read child receipt through blob plane" {
		h.t.Fatalf("parent result: %+v", result)
	}
	counts := map[string]int{}
	for _, event := range qualificationEvents(h) {
		if event.Type == journal.EventRunnerAnnotation {
			kind, _ := event.Runner["kind"].(string)
			counts[kind]++
		}
	}
	if counts[journal.ChildWaitKind] != 1 || counts[journal.ChildContinuedKind] != 1 {
		h.t.Fatalf("wait/continued journal events: %v", counts)
	}
	var children, receipts int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*), count(*) FILTER (WHERE state='completed' AND result_receipt_digest<>'')
		FROM control.child_lineage WHERE instance_id=$1 AND parent_run_id=$2`, testInstance, h.input.RunID).Scan(&children, &receipts); err != nil || children != 1 || receipts != 1 {
		h.t.Fatalf("child custody: children=%d sealed completed=%d (%v)", children, receipts, err)
	}
}
