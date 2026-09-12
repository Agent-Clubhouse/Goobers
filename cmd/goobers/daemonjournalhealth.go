package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
)

const daemonJournalHealthTimeout = 2 * time.Second

func reportLiveDaemonJournalHealth(layout instance.Layout, output io.Writer) {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	health, err := readLiveDaemonJournalHealth(context.Background(), layout, client)
	if err != nil || health == nil || health.AppendsDropped == 0 {
		return
	}
	pf(output, "%s", journalHealthStatusLine(readservice.SchedulerStatus{JournalHealth: health}))
}

// readLiveDaemonJournalHealth uses the daemon's in-memory read service because
// a failed journal cannot durably record its own drop counter. Status treats an
// unavailable/older API as absence so this additive diagnostic never changes
// daemon liveness exit semantics.
func readLiveDaemonJournalHealth(ctx context.Context, layout instance.Layout, client *http.Client) (*readservice.JournalHealthStatus, error) {
	endpoint, err := localDaemonAPIBase(layout)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, daemonJournalHealthTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+apicontract.InstancePath, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if token := strings.TrimSpace(os.Getenv("GOOBERS_API_TOKEN")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, &daemonJournalHealthHTTPError{status: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRemoteTriggerResponseBody+1))
	if err != nil || len(body) > maxRemoteTriggerResponseBody {
		return nil, &daemonJournalHealthHTTPError{}
	}
	var value readservice.Instance
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	expectedID, err := instance.ReadRootIdentity(layout.Root)
	if err != nil {
		return nil, err
	}
	if value.RootIdentity == nil || value.RootIdentity.ID != expectedID {
		return nil, &daemonJournalHealthHTTPError{}
	}
	return value.JournalHealth, nil
}

type daemonJournalHealthHTTPError struct{ status int }

func (e *daemonJournalHealthHTTPError) Error() string {
	if e.status != 0 {
		return http.StatusText(e.status)
	}
	return "daemon journal health unavailable"
}
