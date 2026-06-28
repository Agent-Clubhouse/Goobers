package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// HTTPClient sends HTTP requests for provider implementations.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// CommandRunner executes external commands such as git clone.
type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func httpClientOrDefault(client HTTPClient) HTTPClient {
	if client != nil {
		return client
	}
	return http.DefaultClient
}

func commandRunnerOrDefault(runner CommandRunner) CommandRunner {
	if runner != nil {
		return runner
	}
	return execRunner{}
}

func newJSONRequest(ctx context.Context, method, endpoint string, body interface{}) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func doJSON(client HTTPClient, req *http.Request, out interface{}) error {
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s failed: status %d: %s", req.Method, req.URL.String(), resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func joinURL(base string, elems ...string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", fmt.Errorf("parse base url: %w", err)
	}
	path := strings.TrimRight(u.Path, "/")
	for _, elem := range elems {
		path += "/" + strings.Trim(elem, "/")
	}
	u.Path = path
	return u.String(), nil
}

func addQuery(endpoint string, values url.Values) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	q := u.Query()
	for key, vals := range values {
		for _, val := range vals {
			q.Add(key, val)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func requireRepo(repo RepositoryRef) error {
	if repo.Name == "" && repo.ID == "" {
		return errors.New("repository name or id is required")
	}
	return nil
}

func requireOwnerRepo(repo RepositoryRef) error {
	if repo.Owner == "" {
		return errors.New("repository owner is required")
	}
	if repo.Name == "" {
		return errors.New("repository name is required")
	}
	return nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func statusLabel(status WorkItemStatus) string {
	return "goobers/status:" + string(status)
}

func replaceStatusLabel(labels []string, status WorkItemStatus) []string {
	next := make([]string, 0, len(labels)+1)
	for _, label := range labels {
		if strings.HasPrefix(label, "goobers/status:") {
			continue
		}
		next = append(next, label)
	}
	if status != "" {
		next = append(next, statusLabel(status))
	}
	return uniqueStrings(next)
}

func statusFromLabels(labels []string, fallbackState string) WorkItemStatus {
	for _, label := range labels {
		if strings.HasPrefix(label, "goobers/status:") {
			return WorkItemStatus(strings.TrimPrefix(label, "goobers/status:"))
		}
	}
	switch strings.ToLower(fallbackState) {
	case "closed", "done", "resolved":
		return WorkItemStatusDone
	case "active", "in progress", "in-progress":
		return WorkItemStatusInProgress
	default:
		return WorkItemStatusOpen
	}
}

func basicAuth(username, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+token))
}

func shouldEmitWorkItem(seen map[string]time.Time, item WorkItem) bool {
	updated := time.Time{}
	if item.UpdatedAt != nil {
		updated = *item.UpdatedAt
	}
	previous, ok := seen[item.ID]
	if ok && previous.Equal(updated) {
		return false
	}
	seen[item.ID] = updated
	return true
}

func normalizeCommitChange(changeType string, exists bool) (CommitChangeType, error) {
	switch CommitChangeType(changeType) {
	case "":
		if exists {
			return CommitChangeEdit, nil
		}
		return CommitChangeAdd, nil
	case CommitChangeAdd:
		if exists {
			return "", errors.New("cannot add an existing file")
		}
		return CommitChangeAdd, nil
	case CommitChangeEdit:
		if !exists {
			return "", errors.New("cannot edit a missing file")
		}
		return CommitChangeEdit, nil
	case CommitChangeDelete:
		if !exists {
			return "", errors.New("cannot delete a missing file")
		}
		return CommitChangeDelete, nil
	default:
		return "", fmt.Errorf("unsupported commit change type %q", changeType)
	}
}
